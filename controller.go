package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	basicv1alpha1 "github.com/coseguera/basic-controller/pkg/apis/basiccontroller/v1alpha1"
	clientset "github.com/coseguera/basic-controller/pkg/generated/clientset/versioned"
	basicscheme "github.com/coseguera/basic-controller/pkg/generated/clientset/versioned/scheme"
	informers "github.com/coseguera/basic-controller/pkg/generated/informers/externalversions/basiccontroller/v1alpha1"
	listers "github.com/coseguera/basic-controller/pkg/generated/listers/basiccontroller/v1alpha1"
)

const controllerAgentName = "basic-controller"

const (
	// SuccessSynced is used as part of the Event 'reason' when a Demo is synced
	SuccessSynced = "Synced"

	// MessageResourceSynced is the message used for an Event fired when a Demo
	// is synced successfully
	MessageResourceSynced = "Demo synced successfully"
)

type Controller struct {
	// basicclientset is a clientset for our own API group
	basicclientset clientset.Interface

	demoLister  listers.DemoLister
	demosSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

func NewController(
	kubeclientset kubernetes.Interface,
	basicclientset clientset.Interface,
	deploymentInformer appsinformers.DeploymentInformer,
	demoInformer informers.DemoInformer) *Controller {
	// Create event broadcaster
	// Add basic-controller types to the default Kubernetes Scheme so Events can be
	// logged for basic-controller types.
	utilruntime.Must(basicscheme.AddToScheme(scheme.Scheme))
	klog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		basicclientset: basicclientset,
		demoLister:     demoInformer.Lister(),
		demosSynced:    demoInformer.Informer().HasSynced,
		workqueue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Demos"),
		recorder:       recorder,
	}

	klog.Info("Setting up event handlers")
	// Set up an event handler for when Demo resources change
	demoInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueDemo,
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.enqueueDemo(newObj)
		},
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	klog.Info("Starting Demo controller")

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.demosSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	// Launch specified workers to process Demo resources
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.Info("Started workers")
	<-stopCh
	klog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date than when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Demo resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		klog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Demo resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the Demo resource with this namespace/name
	demo, err := c.demoLister.Demos(namespace).Get(name)
	if err != nil {
		// The Demo resource may no longer exist, in which case we stop processing
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("demo '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	if demo.Spec.Message != demo.Status.LastMessage {
		klog.Infof("demo '%s' spec has message '%s' that is different from last message in status '%s'. Updating...", key, demo.Spec.Message, demo.Status.LastMessage)
		err = c.updateDemoStatus(demo, demo.Spec.Message)
		if err != nil {
			return err
		}
	} else {
		klog.Infof("demo '%s' has not changed its message '%s'. Nothing else to do.", key, demo.Spec.Message)
	}

	c.recorder.Event(demo, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

func (c *Controller) updateDemoStatus(demo *basicv1alpha1.Demo, message string) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// or create a copy manually for better performance
	demoCopy := demo.DeepCopy()
	demoCopy.Status.LastMessage = message
	// If the CustomResourceSubresources feature gate is not enabled,
	// we must use Update instead of UpdateStatus to update the Status block of the Demo resource.
	// UpdateStatus will not allow changes to the Spec of the resource,
	// which is ideal for ensuring nothing other than resource status has been updated.
	_, err := c.basicclientset.BasiccontrollerV1alpha1().Demos(demo.Namespace).UpdateStatus(context.TODO(), demoCopy, metav1.UpdateOptions{})
	return err
}

// enqueueDemo takes a Demo resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Demo.
func (c *Controller) enqueueDemo(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
