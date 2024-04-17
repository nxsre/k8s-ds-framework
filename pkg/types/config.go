package types

import (
	"fmt"
	"github.com/golang/glog"
	"github.com/nxsre/k8s-ds-framework/pkg/k8sclient"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultCfgID = "default"
)

var (
	//DSConfigDir defines the configuration file location
	DSConfigDir = "/etc/ds-config"
)

// InsConfig 自定义实例
type InsConfig struct {
}

// DSConfig defines configuration for a node
type DSConfig struct {
	InsConfigs   map[string]InsConfig `yaml:"apps"`
	NodeSelector map[string]string    `yaml:"nodeSelector"`
}

// DetermineType
// TODO: 这里可以获取特定配置
func DetermineType(name string) string {
	return DefaultCfgID
}

func DetermineConfig() (DSConfig, error) {
	nodeLabels, err := k8sclient.GetNodeLabels()
	if err != nil {
		return DSConfig{}, fmt.Errorf("following error happend when trying to read K8s API server Node object: %s", err)
	}
	return readConfig(nodeLabels)
}

func readConfig(labelMap map[string]string) (DSConfig, error) {
	confs, err := ReadAllConfigs()
	if err != nil {
		return DSConfig{}, err
	}
	for index, conf := range confs {
		if labelMap == nil {
			glog.Infof("Using first configuration file as  config in lieu of missing Node information")
			return conf, nil
		}
		for label, labelValue := range labelMap {
			if value, ok := conf.NodeSelector[label]; ok {
				if value == labelValue {
					glog.Infof("Using configuration file no: %d for  config", index)
					return conf, nil
				}
			}
		}
	}
	return DSConfig{}, fmt.Errorf("no matching  configuration file found for provided nodeSelector labels")
}

// ReadConfigFile reads a configuration file
func ReadConfigFile(name string) (DSConfig, error) {
	file, err := os.ReadFile(name)
	if err != nil {
		return DSConfig{}, fmt.Errorf("could not read conf file: %s, because: %s", name, err)
	}
	var config DSConfig
	err = yaml.Unmarshal([]byte(file), &config)
	if err != nil {
		return DSConfig{}, fmt.Errorf("config file could not be parsed because: %s", err)
	}
	for cfgName, cfgBody := range config.InsConfigs {
		config.InsConfigs[cfgName] = cfgBody
	}
	return config, err
}

// SelectIns returns the exact resourceSet belonging to either the exclusive, shared, or default instance of one InsConfig object
// An empty instanceConfig is returned in case the configuration does not contain the requested type
func (conf DSConfig) SelectIns(prefix string) InsConfig {
	for name, config := range conf.InsConfigs {
		if strings.HasPrefix(name, prefix) {
			return config
		}
	}
	return InsConfig{}
}

// 读取所有配置文件
func ReadAllConfigs() ([]DSConfig, error) {
	files, err := filepath.Glob(filepath.Join(DSConfigDir, "config-*"))
	if err != nil {
		return nil, err
	}
	confs := make([]DSConfig, 0)
	for _, f := range files {
		conf, err := ReadConfigFile(f)
		if err != nil {
			return nil, err
		}
		confs = append(confs, conf)
	}
	return confs, nil
}
