package main

import (
	"flag"
	"os"
	"sync"
	"time"

	"github.com/ghodss/yaml"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
)

const (
	netDirectory    = "/sys/class/net/"
	pciDirectory    = "/sys/bus/pci/devices"
	sriovCapable    = "/sriov_totalvfs"
	sriovConfigured = "/sriov_numvfs"

	nsmLabel = "networkservicemesh.io/sriov"
)

var (
	kubeconfig = flag.String("kubeconfig", "", "Absolute path to the kubeconfig file. Either this or master needs to be set if the provisioner is being run out of cluster.")
	wg         sync.WaitGroup
)

type message struct {
	op        int
	configMap *v1.ConfigMap
}

// VF describes a single instance of VF
type VF struct {
	NetworkService string `yaml:"networkService" json:"networkService"`
	ParentDevice   string `yaml:"parentDevice" json:"parentDevice"`
	VFLocalID      int32  `yaml:"vfLocalID" json:"vfLocalID"`
	VFIODevice     string `yaml:"vfioDevice" json:"vfioDevice"`
}

// VFs is map of ALL found VFs on a specific host kyed by PCI address
type VFs struct {
	vfs map[string]*VF
	sync.RWMutex
}

func newVFs() *VFs {
	v := &VFs{}
	vfs := map[string]*VF{}
	v.vfs = vfs
	return v
}

type configMessage struct {
	op      int
	pciAddr string
	vf      VF
}

type configController struct {
	stopCh       chan struct{}
	configCh     chan configMessage
	k8sClientset *kubernetes.Clientset
	informer     cache.SharedIndexInformer
	vfs          *VFs
}

func newConfigController() *configController {
	c := configController{}
	vfs := newVFs()
	c.vfs = vfs

	return &c
}

func setupInformer(informer cache.SharedIndexInformer, queue workqueue.RateLimitingInterface) {
	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				configMap := obj.(*v1.ConfigMap)
				queue.Add(message{
					op: 1, configMap: configMap})
			},
			UpdateFunc: func(old, new interface{}) {
				configMapNew := new.(*v1.ConfigMap)
				configMapOld := old.(*v1.ConfigMap)
				if configMapNew.ResourceVersion == configMapOld.ResourceVersion {
					return
				}
				queue.Add(message{
					op: 2, configMap: configMapNew})
			},
			DeleteFunc: func(obj interface{}) {
				configMap := obj.(*v1.ConfigMap)
				queue.Add(message{
					op: 3, configMap: configMap})
			},
		},
	)
}

func initConfigController(cc *configController) error {

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	cc.informer = cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.LabelSelector = nsmLabel
				return cc.k8sClientset.CoreV1().ConfigMaps(metav1.NamespaceAll).List(options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = nsmLabel
				return cc.k8sClientset.CoreV1().ConfigMaps(metav1.NamespaceAll).Watch(options)
			},
		},
		&v1.ConfigMap{},
		10*time.Second,
		cache.Indexers{},
	)

	setupInformer(cc.informer, queue)

	go cc.informer.Run(cc.stopCh)
	logrus.Info("Started  config controller shared informer factory.")

	// Wait for the informer caches to finish performing it's initial sync of
	// resources
	if !cache.WaitForCacheSync(cc.stopCh, cc.informer.HasSynced) {
		logrus.Error("Error waiting for informer cache to sync")
	}
	logrus.Info("ConfigController's Informer cache is ready")

	// Read forever from the work queue
	go workforever(cc, queue, cc.informer, cc.stopCh)

	return nil
}

func workforever(cc *configController, queue workqueue.RateLimitingInterface, informer cache.SharedIndexInformer, stopCH chan struct{}) {
	for {
		obj, shutdown := queue.Get()
		msg := obj.(message)
		// If the queue has been shut down, we should exit the work queue here
		if shutdown {
			logrus.Error("shutdown signaled, closing stopChNS")
			close(stopCH)
			return
		}

		func(obj message) {
			defer queue.Done(obj)
			switch obj.op {
			case 1:
				logrus.Infof("Config map add called")
				if err := processConfigMapAdd(cc, obj.configMap); err != nil {
					logrus.Errorf("fail to process configmap %s/%s with error: %+v",
						obj.configMap.ObjectMeta.Namespace,
						obj.configMap.ObjectMeta.Name, err)
				}
			case 2:
				logrus.Info("Config map update called")
			case 3:
				logrus.Info("Config map delete called")
			}
			queue.Forget(obj)
		}(msg)
	}
}

func processConfigMapAdd(cc *configController, configMap *v1.ConfigMap) error {
	logrus.Infof("Processing configmap %s/%s", configMap.ObjectMeta.Namespace, configMap.ObjectMeta.Name)
	vfs := newVFs()
	vfs.Lock()
	defer vfs.Unlock()
	for k, v := range configMap.Data {
		vf := VF{}
		if err := yaml.Unmarshal([]byte(v), &vf); err != nil {
			return err
		}
		vfs.vfs[k] = &vf
	}
	cc.vfs = vfs
	logrus.Infof("Imported: %d VF configuration(s). Sending configuration to serviceController.", cc.vfs)

	// Sending to serviceController configuration for all learned VFs
	for k, vf := range cc.vfs.vfs {
		cc.configCh <- configMessage{op: 0, pciAddr: k, vf: *vf}
	}

	return nil
}

func buildClient() (*kubernetes.Clientset, error) {
	k8sClientConfig, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return nil, err

	}
	k8sClientset, err := kubernetes.NewForConfig(k8sClientConfig)
	if err != nil {
		return nil, err
	}
	return k8sClientset, nil
}

func main() {
	flag.Set("logtostderr", "true")
	flag.Parse()

	// Instantiating config controller
	cc := newConfigController()
	cc.stopCh = make(chan struct{})
	configCh := make(chan configMessage)
	cc.configCh = configCh
	k8sClientset, err := buildClient()
	if err != nil {
		logrus.Errorf("Failed to build kubernetes client set with error: %+v ", err)
		os.Exit(1)
	}
	cc.k8sClientset = k8sClientset

	// Instantiating service controller
	sc := newServiceController()
	sc.configCh = configCh
	sc.stopCh = make(chan struct{})
	wg.Add(1)
	// Call configController further configuraion and start
	go initConfigController(cc)
	go sc.Run()

	wg.Wait()
}
