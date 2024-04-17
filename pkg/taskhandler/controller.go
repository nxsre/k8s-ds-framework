package taskhandler

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nxsre/k8s-ds-framework/pkg/checkpoint"
	"github.com/nxsre/k8s-ds-framework/pkg/k8sclient"
	"github.com/nxsre/k8s-ds-framework/pkg/types"
	"golang.org/x/sys/unix"
	"io"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const (
	//MaxRetryCount controls how many times we re-try a remote API operation
	MaxRetryCount = 150
	//RetryInterval controls how much time (in milliseconds) we wait between two retry attempts when talking to a remote API
	RetryInterval = 200
)

var (
	resourceBaseName       = "bonc.k8s.io"
	setterAnnotationSuffix = "cgroup-rules-devices-configured"
	setterAnnotationKey    = resourceBaseName + "/" + setterAnnotationSuffix
	containerPrefixList    = []string{"docker://", "containerd://"}
)

type workItem struct {
	oldPod *v1.Pod
	newPod *v1.Pod
}

// SetHandler is the data set encapsulating the configuration data needed for the resourceSetter Controller to be able to adjust resource
type SetHandler struct {
	dsConfig        types.DSConfig
	resourceFsRoot  string
	k8sClient       kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	podSynced       cache.InformerSynced
	addWorkQueue    workqueue.Interface
	updateWorkQueue workqueue.Interface
	deleteWorkQueue workqueue.Interface
	stopChan        *chan struct{}
}

// SetHandler returns the SetHandler data set
func (setHandler SetHandler) SetHandler() SetHandler {
	return setHandler
}

// SetSetHandler a setter for SetHandler
func (setHandler *SetHandler) SetSetHandler(dsconf types.DSConfig, resourceFsRoot string, k8sClient kubernetes.Interface) {
	setHandler.dsConfig = dsconf
	setHandler.resourceFsRoot = resourceFsRoot
	setHandler.k8sClient = k8sClient
	setHandler.addWorkQueue = workqueue.New()
	setHandler.updateWorkQueue = workqueue.New()
	setHandler.deleteWorkQueue = workqueue.New()
}

// New creates a new SetHandler object
// Can return error if in-cluster K8s API server client could not be initialized
func New(kubeConf string, dsConfig types.DSConfig, resourceFsRoot string) (*SetHandler, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeConf)
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, time.Second)
	podInformer := kubeInformerFactory.Core().V1().Pods().Informer()
	setHandler := SetHandler{
		dsConfig:        dsConfig,
		resourceFsRoot:  resourceFsRoot,
		k8sClient:       kubeClient,
		informerFactory: kubeInformerFactory,
		podSynced:       podInformer.HasSynced,
		addWorkQueue:    workqueue.New(),
		updateWorkQueue: workqueue.New(),
		deleteWorkQueue: workqueue.New(),
	}
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			setHandler.PodAdded((reflect.ValueOf(obj).Interface().(*v1.Pod)))
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			setHandler.PodChanged(reflect.ValueOf(oldObj).Interface().(*v1.Pod), reflect.ValueOf(newObj).Interface().(*v1.Pod))
		},
		DeleteFunc: func(obj interface{}) {
			setHandler.PodDelete((reflect.ValueOf(obj).Interface().(*v1.Pod)))
		},
	})
	podInformer.SetWatchErrorHandler(setHandler.WatchErrorHandler)
	return &setHandler, nil
}

// Run kicks the resourceSetter controller into motion, synchs it with the API server, and starts the desired number of asynch worker threads to handle the Pod API events
func (setHandler *SetHandler) Run(threadiness int, stopCh *chan struct{}) error {
	setHandler.stopChan = stopCh
	setHandler.informerFactory.Start(*stopCh)
	log.Println("INFO: Starting resourceSetter Controller...")
	log.Println("INFO: Waiting for Pod Controller cache to sync...")
	if ok := cache.WaitForCacheSync(*stopCh, setHandler.podSynced); !ok {
		return errors.New("failed to sync Pod Controller from cache! Are you sure everything is properly connected?")
	}
	log.Println("INFO: Starting " + strconv.Itoa(threadiness) + " resourceSetter worker threads...")
	for i := 0; i < threadiness; i++ {
		go wait.Until(setHandler.runWorker, time.Second, *stopCh)
	}
	setHandler.StartReconciliation()
	log.Println("INFO: resourceSetter is successfully initialized, worker threads are now serving requests!")
	return nil
}

// PodAdded handles ADD operations
func (setHandler *SetHandler) PodAdded(pod *v1.Pod) {
	workItem := workItem{newPod: pod}
	setHandler.addWorkQueue.Add(workItem)
}

// PodChanged handles UPDATE operations
func (setHandler *SetHandler) PodChanged(oldPod, newPod *v1.Pod) {
	//The maze wasn't meant for you either
	// 状态发生变化才需要放入更新队列
	if oldPod.Status.Phase != newPod.Status.Phase {
		log.Println("oldPod ----> newPod namespace:%s  podname:%s: %s --> %s", oldPod.Namespace, oldPod.Name, oldPod.Status.Phase, newPod.Status.Phase)
		workItem := workItem{newPod: newPod, oldPod: oldPod}
		setHandler.updateWorkQueue.Add(workItem)
	}
}

// PodDelete handles DELETE operations
func (setHandler *SetHandler) PodDelete(pod *v1.Pod) {
	//The maze wasn't meant for you either
	workItem := workItem{newPod: pod}
	setHandler.updateWorkQueue.Add(workItem)
}

// WatchErrorHandler is an event handler invoked when the resourceSetter Controller's connection to the K8s API server breaks
// In case the error is terminal it initiates a graceful shutdown for the whole Controller, implicitly restarting the connection by restarting the whole container
func (setHandler *SetHandler) WatchErrorHandler(r *cache.Reflector, err error) {
	if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) || err == io.EOF {
		log.Println("INFO: One of the API watchers closed gracefully, re-establishing connection")
		return
	}
	//The default K8s client retry mechanism expires after a certain amount of time, and just gives-up
	//It is better to shutdown the whole process now and freshly re-build the watchers, rather than risking becoming a permanent zombie
	log.Println("ERROR: One of the API watchers closed unexpectedly with error:" + err.Error() + " restarting resourceSetter!")
	setHandler.Stop()
	//Give some time for gracefully terminating the connections
	time.Sleep(5 * time.Second)
	os.Exit(0)
}

// Stop is invoked by the main thread to initiate graceful shutdown procedure. It shuts down the event handler queue, and relays a stop signal to the Controller
func (setHandler *SetHandler) Stop() {
	*setHandler.stopChan <- struct{}{}
	setHandler.addWorkQueue.ShutDown()
	setHandler.updateWorkQueue.ShutDown()
	setHandler.deleteWorkQueue.ShutDown()
}

// StartReconciliation starts the reactive thread of SetHandler periodically checking expected and provisioned resourceSet of the node
// In case a container's observed resourceSet differs from the expected (i.e. container was restarted) the thread resets it to the proper value
func (setHandler *SetHandler) StartReconciliation() {
	go setHandler.startReconciliationLoop()
	log.Println("INFO: Successfully started the periodic resourceSet reconciliation thread")
}

func (setHandler *SetHandler) runWorker() {
	go func() {
		for setHandler.processNextWorkAddItem() {
		}
	}()
	go func() {
		for setHandler.processNextWorkUpdateItem() {
		}
	}()
	go func() {
		for setHandler.processNextWorkDeleteItem() {
		}
	}()
	<-*setHandler.stopChan
}

func (setHandler *SetHandler) processNextWorkAddItem() bool {
	obj, areWeShuttingDown := setHandler.addWorkQueue.Get()
	if areWeShuttingDown {
		log.Println("WARNING: Received shutdown command from queue in thread:" + strconv.Itoa(unix.Gettid()))
		return false
	}
	setHandler.processAddItemInQueue(obj)
	return true
}

func (setHandler *SetHandler) processNextWorkUpdateItem() bool {
	obj, areWeShuttingDown := setHandler.updateWorkQueue.Get()
	if areWeShuttingDown {
		log.Println("WARNING: Received shutdown command from queue in thread:" + strconv.Itoa(unix.Gettid()))
		return false
	}
	setHandler.processUpdateItemInQueue(obj)
	return true
}

func (setHandler *SetHandler) processNextWorkDeleteItem() bool {
	obj, areWeShuttingDown := setHandler.deleteWorkQueue.Get()
	if areWeShuttingDown {
		log.Println("WARNING: Received shutdown command from queue in thread:" + strconv.Itoa(unix.Gettid()))
		return false
	}
	setHandler.processDeeteItemInQueue(obj)
	return true
}

func (setHandler *SetHandler) processAddItemInQueue(obj interface{}) {
	defer setHandler.addWorkQueue.Done(obj)
	var item workItem
	var ok bool
	if item, ok = obj.(workItem); !ok {
		log.Println("WARNING: Cannot decode work item from queue in thread: " + strconv.Itoa(unix.Gettid()) + ", be aware that we are skipping some events!!!")
		return
	}
	setHandler.handleAddPods(item)
}

func (setHandler *SetHandler) processUpdateItemInQueue(obj interface{}) {
	defer setHandler.updateWorkQueue.Done(obj)
	var item workItem
	var ok bool
	if item, ok = obj.(workItem); !ok {
		log.Println("WARNING: Cannot decode work item from queue in thread: " + strconv.Itoa(unix.Gettid()) + ", be aware that we are skipping some events!!!")
		return
	}
	setHandler.handleUpdatePods(item)
}

func (setHandler *SetHandler) processDeeteItemInQueue(obj interface{}) {
	defer setHandler.deleteWorkQueue.Done(obj)
	var item workItem
	var ok bool
	if item, ok = obj.(workItem); !ok {
		log.Println("WARNING: Cannot decode work item from queue in thread: " + strconv.Itoa(unix.Gettid()) + ", be aware that we are skipping some events!!!")
		return
	}
	setHandler.handleDeletePods(item)
}

func (setHandler *SetHandler) handleAddPods(item workItem) {
	isItMyPod, pod := shouldPodBeHandled(*item.newPod)
	//The maze wasn't meant for you
	if !isItMyPod {
		return
	}
	log.Println("新增", pod.Namespace, pod.Name, pod.Status.Phase, pod.Status.StartTime)

	containersToBeSet := gatherAllContainers(pod)
	if len(containersToBeSet) > 0 {
		var err error
		for i := 0; i < MaxRetryCount; i++ {
			err = setHandler.adjustContainerSets(pod, containersToBeSet)
			if err == nil {
				return
			}
			time.Sleep(RetryInterval * time.Millisecond)
		}
		log.Println("ERROR: Timed out trying to adjust the resourceSets of the containers belonging to Pod:" + pod.ObjectMeta.Name + " ID: " + string(pod.ObjectMeta.UID) + " because:" + err.Error())
	} else {
		log.Println("WARNING: there were no containers to handle in: " + pod.ObjectMeta.Name + " ID: " + string(pod.ObjectMeta.UID) + " in thread:" + strconv.Itoa(unix.Gettid()))
	}
}

func (setHandler *SetHandler) handleUpdatePods(item workItem) {
	isItMyPod, pod := shouldPodBeHandled(*item.newPod)
	//The maze wasn't meant for you
	if !isItMyPod {
		return
	}
	log.Println("更新", pod.Namespace, pod.Name, pod.Status.Phase, pod.Status.StartTime)
}

func (setHandler *SetHandler) handleDeletePods(item workItem) {
	isItMyPod, pod := shouldPodBeHandled(*item.newPod)
	//The maze wasn't meant for you
	if !isItMyPod {
		return
	}
	log.Println("删除", pod.Namespace, pod.Name, pod.Status.Phase, pod.Status.StartTime)
}

func shouldPodBeHandled(pod v1.Pod) (bool, v1.Pod) {
	// Pod has exited/completed and all containers have stopped
	log.Println("pod 状态:", pod.Namespace, pod.Name, pod.Status.Phase)
	if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		return false, pod
	}
	setterNodeName := os.Getenv("NODE_NAME")
	for i := 0; i < MaxRetryCount; i++ {
		//We will unconditionally read the Pod at least once due to two reasons:
		//1: 99% Chance that the Pod arriving in the CREATE event is not yet ready to be processed
		//2: Avoid spending cycles on a Pod which does not even exist anymore in the API server
		newPod, err := k8sclient.RefreshPod(pod)
		if err != nil {
			log.Println("WARNING: Pod:" + pod.ObjectMeta.Name + " ID: " + string(pod.ObjectMeta.UID) + " is not adjusted as reading it again failed with:" + err.Error())
			return false, pod
		}
		if isPodReadyForProcessing(*newPod) {
			pod = *newPod
			break
		}
		time.Sleep(RetryInterval * time.Millisecond)
	}
	//Pod still haven't been scheduled, or it wasn't scheduled to the Node of this specific resourceSetter instance
	if setterNodeName != pod.Spec.NodeName {
		return false, pod
	}
	return true, pod
}

func isPodReadyForProcessing(pod v1.Pod) bool {
	if pod.Spec.NodeName == "" || len(pod.Status.ContainerStatuses) != len(pod.Spec.Containers) {
		return false
	}
	for _, cStatus := range pod.Status.ContainerStatuses {
		if cStatus.ContainerID == "" {
			//Pod might have been scheduled but its containers haven't been created yet
			return false
		}
	}
	return true
}

func gatherAllContainers(pod v1.Pod) map[string]int {
	workingContainers := map[string]int{}
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.ContainerID == "" {
			return map[string]int{}
		}
		workingContainers[containerStatus.Name] = 0
	}
	return workingContainers
}

func (setHandler *SetHandler) adjustContainerSets(pod v1.Pod, containersToBeSet map[string]int) error {
	var (
		pathToContainerFile string
		err                 error
	)
	for _, container := range pod.Spec.Containers {
		if _, found := containersToBeSet[container.Name]; !found {
			continue
		}

		containerID := determineCid(pod.Status, container.Name)
		if containerID == "" {
			return errors.New("cannot determine container ID of container: " + container.Name + " in Pod: " + pod.ObjectMeta.Name + " ID: " + string(pod.ObjectMeta.UID) + " in thread:" + strconv.Itoa(unix.Gettid()) + " because:" + err.Error())
		}

		// 实现自定义的逻辑，并返回 pathToContainerFile,供后续调用
		pathToContainerFile = ""

	}
	err = setHandler.applyResourceSetToInfraContainer(pod.ObjectMeta, pod.Status, pathToContainerFile)
	if err != nil {
		return errors.New("resource of the infra container in Pod: " + pod.ObjectMeta.Name + " ID: " + string(pod.ObjectMeta.UID) + " could not be re-adjusted in thread:" + strconv.Itoa(unix.Gettid()) + " because:" + err.Error())
	}

	// 操作完成更新 pod annotation
	err = k8sclient.SetPodAnnotation(pod, setterAnnotationKey, "true")
	if err != nil {
		return errors.New("could not update annotation in Pod:" + pod.ObjectMeta.Name + " ID: " + string(pod.ObjectMeta.UID) + "  in thread:" + strconv.Itoa(unix.Gettid()) + " because: " + err.Error())
	}
	return nil
}

// getListOfAllocated 解析 getListOfAllocated，获取设备分配信息
func (setHandler *SetHandler) getListOfAllocated(resourceName string, pod v1.Pod, container v1.Container) error {
	checkpointFileName := "/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint"
	buf, err := os.ReadFile(checkpointFileName)
	if err != nil {
		log.Printf("Error reading file %s: Error: %v", checkpointFileName, err)
		return fmt.Errorf("kubelet checkpoint file could not be accessed because: %s", err)
	}
	var cp checkpoint.File
	if err = json.Unmarshal(buf, &cp); err != nil {
		//K8s 1.21 changed internal file structure, so let's try that too before returning with error
		var newCpFile checkpoint.NewFile
		if err = json.Unmarshal(buf, &newCpFile); err != nil {
			log.Printf("error unmarshalling kubelet checkpoint file: %s", err)
			return err
		}
		cp = checkpoint.TranslateNewCheckpointToOld(newCpFile)
	}
	podIDStr := string(pod.ObjectMeta.UID)
	deviceIDs := []string{}
	for _, entry := range cp.Data.PodDeviceEntries {
		// 如果 podUID 、containerName、ResourceName 全部匹配，证明找到了预期的设备分配信息
		if entry.PodUID == podIDStr && entry.ContainerName == container.Name &&
			entry.ResourceName == resourceName {
			deviceIDs = append(deviceIDs, entry.DeviceIDs...)
		}
		// 如果 resourceName 为空，打印 debug 信息
		if resourceName == "" {
			slog.Debug(resourceBaseName)
		}
	}
	if len(deviceIDs) == 0 {
		log.Printf("WARNING: Container: %s in Pod: %s asked for <resource>, but were not allocated any! Cannot adjust its default resourceSet", container.Name, podIDStr)
		return nil
	}
	return nil
}

func determineCid(podStatus v1.PodStatus, containerName string) string {
	for _, containerStatus := range podStatus.ContainerStatuses {
		if containerStatus.Name == containerName {
			return trimContainerPrefix(containerStatus.ContainerID)
		}
	}
	return ""
}

func trimContainerPrefix(contName string) string {
	for _, prefix := range containerPrefixList {
		if strings.HasPrefix(contName, prefix) {
			return strings.TrimPrefix(contName, prefix)
		}
	}
	return contName
}

func containerIDInPodStatus(podStatus v1.PodStatus, containerDirName string) bool {
	for _, containerStatus := range podStatus.ContainerStatuses {
		trimmedCid := trimContainerPrefix(containerStatus.ContainerID)
		if strings.Contains(containerDirName, trimmedCid) {
			return true
		}
	}
	return false
}

// applyToContainer 实现应用到容器内的逻辑
func (setHandler *SetHandler) applyToContainer(podMeta metav1.ObjectMeta, containerID string) (string, error) {
	return "", nil
}

func getInfraContainerPath(podStatus v1.PodStatus, searchPath string) string {
	var pathToInfraContainer string
	filelist, _ := filepath.Glob(filepath.Dir(searchPath) + "/*")
	for _, fpath := range filelist {
		fstat, err := os.Stat(fpath)
		if err != nil {
			continue
		}
		if fstat.IsDir() && !containerIDInPodStatus(podStatus, fstat.Name()) {
			pathToInfraContainer = fpath
		}
	}
	return pathToInfraContainer
}

// 应用一些资源设置到容器中
func (setHandler *SetHandler) applyResourceSetToInfraContainer(podMeta metav1.ObjectMeta, podStatus v1.PodStatus, pathToSearchContainer string) error {
	// 根据配置类型 ID 获取实例化配置
	instanceConfig := setHandler.dsConfig.SelectIns(types.DefaultCfgID)
	_ = instanceConfig
	if pathToSearchContainer == "" {
		return fmt.Errorf("container directory does not exists under the provided cgroupfs hierarchy: %s", setHandler.resourceFsRoot)
	}
	pathToContainerFile := getInfraContainerPath(podStatus, pathToSearchContainer)
	if pathToContainerFile == "" {
		return fmt.Errorf("resource file does not exist for infra container under the provided cgroupfs hierarchy: %s", setHandler.resourceFsRoot)
	}
	// 这里可以做一些操作，比如写入 cgroup devices 权限
	return nil
}

func (setHandler *SetHandler) startReconciliationLoop() {
	timeToReconcile := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-timeToReconcile.C:
			err := setHandler.reconcileResource()
			if err != nil {
				log.Println("WARNING: Periodic resource reconciliation failed with error:" + err.Error())
				continue
			}
		case <-*setHandler.stopChan:
			log.Println("INFO: Shutting down the periodic resource reconciliation thread")
			timeToReconcile.Stop()
			return
		}
	}
}

func (setHandler *SetHandler) reconcileResource() error {
	// 获取当前节点上的 Pod
	pods, err := k8sclient.GetMyPods()
	if pods == nil || err != nil {
		return errors.New("couldn't List my Pods in the reconciliation loop because:" + err.Error())
	}

	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			err = setHandler.reconcileContainer([]string{}, pod, container)
			if err != nil {
				log.Println("WARNING: Periodic reconciliation of container:" + container.Name + " of Pod:" + pod.ObjectMeta.Name + " in namespace:" + pod.ObjectMeta.Namespace + " failed with error:" + err.Error())
			}
		}
	}
	return nil
}

// Naive approach: we can prob afford not building a tree from the cgroup paths if we only reconcile every couple of seconds
// Can be further optimized on need
func (setHandler *SetHandler) reconcileContainer(leafResource []string, pod v1.Pod, container v1.Container) error {
	containerID := determineCid(pod.Status, container.Name)
	if containerID == "" {
		return nil
	}
	return nil
}
