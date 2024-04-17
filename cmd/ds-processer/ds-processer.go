package main

import (
	"flag"
	"github.com/nxsre/k8s-ds-framework/pkg/taskhandler"
	"github.com/nxsre/k8s-ds-framework/pkg/types"
	"log"
	"os"
	"os/signal"
	"syscall"
)

const (
	//NumberOfWorkers controls how many asynch event handler threads are started in the resourceSetter controller
	NumberOfWorkers = 100
)

var (
	kubeConfig     string
	configPath     string
	resourceFsRoot string // 要操作的目标目录，比如 cgroupsPath /rootfs/sys/fs/cgroup/devices/
)

func main() {
	flag.Parse()
	if configPath == "" || resourceFsRoot == "" {
		log.Fatal("ERROR: Mandatory command-line arguments configs and rootfs were not provided!")
	}
	Conf, err := types.DetermineConfig()
	if err != nil {
		log.Fatal("ERROR: Could not read configuration files because: " + err.Error() + ", exiting!")
	}
	setHandler, err := taskhandler.New(kubeConfig, Conf, resourceFsRoot)
	if err != nil {
		log.Fatal("ERROR: Could not initalize K8s client because of error: " + err.Error() + ", exiting!")
	}

	stopChannel := make(chan struct{})
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)
	log.Println("resourceSetter's Controller initalized successfully!")
	setHandler.Run(NumberOfWorkers, &stopChannel)
	select {
	case <-signalChannel:
		log.Println("Orchestrator initiated graceful shutdown, ending resourceSetter workers...(o_o)/")
		setHandler.Stop()
	}
}

func init() {
	flag.StringVar(&configPath, "configs", "", "Path to the configuration files. Mandatory parameter.")
	flag.StringVar(&resourceFsRoot, "rootfs", "", "The root of the cgroupfs where Kubernetes creates the resource for the Pods . Mandatory parameter.")
	flag.StringVar(&kubeConfig, "kubeconfig", "", "Path to a kubeconfig. Optional parameter, only required if out-of-cluster.")
}
