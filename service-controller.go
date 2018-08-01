package main

import (
	"sync"

	"github.com/sirupsen/logrus"
)

type serviceInstance struct {
	vfs      map[string]*VF
	stopCh   chan struct{}
	configCh chan configMessage
}

// first key is network service, second key is pci address of VF
type serviceController struct {
	sriovNetServices map[string]serviceInstance
	configCh         chan configMessage
	// to shut down controller
	stopCh chan struct{}
	// locking for the time of changes
	sync.RWMutex
}

func newServiceController() *serviceController {
	sc := map[string]serviceInstance{}
	return &serviceController{
		sriovNetServices: sc}
}

func (s *serviceController) Run() {
	logrus.Infof("Service Controller is ready, waiting for config messages...")
	for {
		select {
		case <-s.stopCh:
			// global shutdown exiting wait loop
			logrus.Infof("Received global shutdown messages, shutting down all service instances...")
			s.Stop()
			return
		case msg := <-s.configCh:
			switch msg.op {
			case operationAdd:
				logrus.Infof("Service Controller: Received config message to add network service: %s pci address: %s", msg.vf.NetworkService, msg.pciAddr)
				s.processAdd(msg)
			case operationDel:
				logrus.Info("Service Controller: Received Delete operation, shutting down service controller")
				s.Stop()
			case operationUpdate:
				logrus.Infof("Service Controller: Received config message to update network service: %s", msg.vf.NetworkService)
				s.processUpdate(msg)
			default:
				logrus.Errorf("error, recevied message with unknown operation %d", msg.op)
			}
		}
	}
}

func (s *serviceController) processAdd(msg configMessage) {
	// Check if there is already an instance of network service
	_, ok := s.sriovNetServices[msg.vf.NetworkService]
	if !ok {
		// Network Service instance is not found, need to instantiate one
		logrus.Infof("Creating new Network Service instance for Network Service: %s", msg.vf.NetworkService)
		vfs := map[string]*VF{}
		vfs[msg.pciAddr] = &msg.vf
		si := serviceInstance{
			vfs:      vfs,
			configCh: make(chan configMessage),
			stopCh:   make(chan struct{}),
		}
		s.sriovNetServices[msg.vf.NetworkService] = si
		// Instantiating Service Instance controller
		sic := newServiceInstanceController()
		sic.configCh = si.configCh
		sic.stopCh = si.stopCh
		go sic.Run()
	}
	// Network Service instance already exists, just need to inform about new VF
	nsi := s.sriovNetServices[msg.vf.NetworkService]
	nsi.configCh <- msg
}

func (s *serviceController) processUpdate(msg configMessage) {
	// Check if there is already an instance of network service
	_, ok := s.sriovNetServices[msg.vf.NetworkService]
	if !ok {
		// Network Service instance is not found
		logrus.Errorf("fatal error as received update message for non-existing network service %s, ignoring it", msg.vf.NetworkService)
		return
	}
	// Network Service instance already exists, just need to inform about new VF
	nsi := s.sriovNetServices[msg.vf.NetworkService]
	nsi.vfs = map[string]*VF{}
	nsi.configCh <- msg
}

func (s *serviceController) Stop() {
	// Inform all network service instances to shut down
	for _, ns := range s.sriovNetServices {
		ns.stopCh <- struct{}{}
	}
}
