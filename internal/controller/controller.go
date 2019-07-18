/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	prowjobv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	prowjobset "k8s.io/test-infra/prow/client/clientset/versioned"
	prowjobscheme "k8s.io/test-infra/prow/client/clientset/versioned/scheme"
	prowjobinfov1 "k8s.io/test-infra/prow/client/informers/externalversions/prowjobs/v1"
	prowjoblisters "k8s.io/test-infra/prow/client/listers/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
	"k8s.io/test-infra/prow/pod-utils/decorate"
	"k8s.io/test-infra/prow/pod-utils/downwardapi"

	jsonpatch "github.com/evanphx/json-patch"
	duckv1alpha1 "github.com/knative/pkg/apis/duck/v1alpha1"
	"github.com/sirupsen/logrus"
	pipelinev1alpha1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	pipelineset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	pipelineinfov1alpha1 "github.com/tektoncd/pipeline/pkg/client/informers/externalversions/pipeline/v1alpha1"
	untypedcorev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

const (
	// ControllerName is name of the Controller
	ControllerName = "prow-pipeline-crd"
	prowJobName    = "prowJobName"
	pipelineRun    = "PipelineRun"
	prowJob        = "ProwJob"

	// Abort pipeline run request which don't return in 5 mins.
	maxPipelineRunRequestTimeout = 5 * time.Minute
)

// PipelineConfig contains the required config to run the Controller.
type PipelineConfig struct {
	Client   pipelineset.Interface
	Informer pipelineinfov1alpha1.PipelineRunInformer
}

// Controller defines all the required logic for the pipeline controller.
type Controller struct {
	config    config.Getter
	pjc       prowjobset.Interface
	pipelines map[string]PipelineConfig
	totURL    string

	pjLister   prowjoblisters.ProwJobLister
	pjInformer cache.SharedIndexInformer

	workqueue workqueue.RateLimitingInterface

	recorder record.EventRecorder

	prowJobsDone  bool
	pipelinesDone map[string]bool
	wait          string

	log *logrus.Entry
}

// PipelineRunRequest request data sent to an external service which starts the pipeline
type PipelineRunRequest struct {
	Labels      map[string]string     `json:"labels,omitempty"`
	ProwJobSpec prowjobv1.ProwJobSpec `json:"prowJobSpec,omitempty"`
}

// PipelineRunResponse reponse data received from the external service which starts the pipeline
type PipelineRunResponse struct {
	Resources []ObjectReference `json:"resources,omitempty"`
}

// ObjectReference represents a reference to a k8s resource
type ObjectReference struct {
	APIVersion string `json:"apiVersion" protobuf:"bytes,5,opt,name=apiVersion"`
	// Kind of the referent.
	// More info: https://git.k8s.io/community/contributors/devel/api-conventions.md#types-kinds
	Kind string `json:"kind" protobuf:"bytes,1,opt,name=kind"`
	// Name of the referent.
	// More info: http://kubernetes.io/docs/user-guide/identifiers#names
	Name string `json:"name" protobuf:"bytes,3,opt,name=name"`
}

// pjNamespace retruns the prow namespace from configuration
func (c *Controller) pjNamespace() string {
	return c.config().ProwJobNamespace
}

// PipelineRunnerURL returns the URL of the pipeline runner service
func (c *Controller) pipelineRunnerURL() string {
	return "http://pipelinerunner"
}

// hasSynced returns true when every prowjob and pipeline Informer has synced.
func (c *Controller) hasSynced() bool {
	if !c.pjInformer.HasSynced() {
		if c.wait != "prowjobs" {
			c.wait = "prowjobs"
			ns := c.pjNamespace()
			if ns == "" {
				ns = "controllers"
			}
			logrus.Infof("Waiting on prowjobs in %s namespace...", ns)
		}
		return false // still syncing prowjobs
	}
	if !c.prowJobsDone {
		c.prowJobsDone = true
		logrus.Info("Synced prow jobs")
	}
	if c.pipelinesDone == nil {
		c.pipelinesDone = map[string]bool{}
	}
	for n, cfg := range c.pipelines {
		if !cfg.Informer.Informer().HasSynced() {
			if c.wait != n {
				c.wait = n
				logrus.Infof("Waiting on %s pipelines...", n)
			}
			return false // still syncing pipelines in at least one cluster
		} else if !c.pipelinesDone[n] {
			c.pipelinesDone[n] = true
			logrus.Infof("Synced %s pipelines", n)
		}
	}
	return true // Everyone is synced
}

// NewController creates a new instance of this Controller
func NewController(kc kubernetes.Interface, pjc prowjobset.Interface, pji prowjobinfov1.ProwJobInformer, pipelineConfigs map[string]PipelineConfig,
	totURL string, prowConfig config.Getter, rl workqueue.RateLimitingInterface, logger *logrus.Entry) (*Controller, error) {
	// Log to events
	err := prowjobscheme.AddToScheme(scheme.Scheme)
	if err != nil {
		return nil, err
	}
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: kc.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, untypedcorev1.EventSource{Component: ControllerName})

	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}

	c := &Controller{
		config:     prowConfig,
		pjc:        pjc,
		pipelines:  pipelineConfigs,
		pjLister:   pji.Lister(),
		pjInformer: pji.Informer(),
		workqueue:  rl,
		recorder:   recorder,
		totURL:     totURL,
		log:        logger,
	}

	logrus.Info("Setting up event handlers")

	// Reconcile whenever a prowjob changes
	pji.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pj, ok := obj.(*prowjobv1.ProwJob)
			if !ok {
				logrus.Warnf("Ignoring bad prowjob add: %v", obj)
				return
			}
			c.enqueueKey(pj.Spec.Cluster, pj)
		},
		UpdateFunc: func(old, new interface{}) {
			pj, ok := new.(*prowjobv1.ProwJob)
			if !ok {
				logrus.Warnf("Ignoring bad prowjob update: %v", new)
				return
			}
			c.enqueueKey(pj.Spec.Cluster, pj)
		},
		DeleteFunc: func(obj interface{}) {
			pj, ok := obj.(*prowjobv1.ProwJob)
			if !ok {
				logrus.Warnf("Ignoring bad prowjob delete: %v", obj)
				return
			}
			c.enqueueKey(pj.Spec.Cluster, pj)
		},
	})

	for ctx, cfg := range pipelineConfigs {
		// Reconcile whenever a pipelinerun changes.
		ctx := ctx // otherwise it will change
		cfg.Informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				c.enqueueKey(ctx, obj)
			},
			UpdateFunc: func(old, new interface{}) {
				c.enqueueKey(ctx, new)
			},
			DeleteFunc: func(obj interface{}) {
				c.enqueueKey(ctx, obj)
			},
		})
	}

	return c, nil
}

// Run starts threads workers, returning after receiving a stop signal.
func (c *Controller) Run(threads int, stop <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	logrus.Info("Starting Pipeline Controller")
	logrus.Info("Waiting for Informer caches to sync")
	if ok := cache.WaitForCacheSync(stop, c.hasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logrus.Info("Starting workers")
	for i := 0; i < threads; i++ {
		go wait.Until(c.runWorker, time.Second, stop)
	}

	logrus.Info("Started workers")
	<-stop
	logrus.Info("Shutting down workers")
	return nil
}

// runWorker dequeues to reconcile, until the queue has closed.
func (c *Controller) runWorker() {
	for {
		key, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}
		func() {
			defer c.workqueue.Done(key)

			if err := reconcile(c, key.(string)); err != nil {
				runtime.HandleError(fmt.Errorf("failed to reconcile %s: %v", key, err))
				return // Do not forget so we retry later.
			}
			c.workqueue.Forget(key)
		}()
	}
}

// toKey returns context/namespace/name/kind
func toKey(ctx, namespace, name, kind string) string {
	return strings.Join([]string{ctx, namespace, name, kind}, "/")
}

// fromKey converts toKey back into its parts
func fromKey(key string) (string, string, string, string, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("bad key: %q", key)
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}

// enqueueKey schedules an item for reconciliation
func (c *Controller) enqueueKey(ctx string, obj interface{}) {
	switch o := obj.(type) {
	case *prowjobv1.ProwJob:
		ns := o.Spec.Namespace
		if ns == "" {
			ns = o.Namespace
		}
		c.workqueue.AddRateLimited(toKey(ctx, ns, o.Name, prowJob))
	case *pipelinev1alpha1.PipelineRun:
		c.workqueue.AddRateLimited(toKey(ctx, o.Namespace, o.Name, pipelineRun))
	default:
		logrus.Warnf("cannot enqueue unknown type %T: %v", o, obj)
		return
	}
}

type reconciler interface {
	getProwJob(name string) (*prowjobv1.ProwJob, error)
	patchProwJob(pj *prowjobv1.ProwJob) error
	getPipelineRun(context, namespace, name string) (*pipelinev1alpha1.PipelineRun, error)
	getPipelineRunsWithSelector(context, namespace, selector string) ([]*pipelinev1alpha1.PipelineRun, error)
	deletePipelineRun(context, namespace, name string) error
	createPipelineRun(context, namespace string, b *pipelinev1alpha1.PipelineRun) (*pipelinev1alpha1.PipelineRun, error)
	createPipelineResource(context, namespace string, b *pipelinev1alpha1.PipelineResource) (*pipelinev1alpha1.PipelineResource, error)
	pipelineID(prowjobv1.ProwJob) (string, error)
	requestPipelineRun(context, namespace string, pj prowjobv1.ProwJob) (string, error)
	now() metav1.Time
	getProwJobURL(prowjobv1.ProwJob) string
}

func (c *Controller) getPipelineConfig(ctx string) (PipelineConfig, error) {
	cfg, ok := c.pipelines[ctx]
	if !ok {
		defaultCtx := kube.DefaultClusterAlias
		defaultCfg, ok := c.pipelines[defaultCtx]
		if !ok {
			return PipelineConfig{}, fmt.Errorf("no cluster configuration found for default context %q", defaultCtx)
		}
		return defaultCfg, nil
	}
	return cfg, nil
}

func (c *Controller) getProwJob(name string) (*prowjobv1.ProwJob, error) {
	return c.pjLister.ProwJobs(c.pjNamespace()).Get(name)
}

func (c *Controller) updateProwJob(pj *prowjobv1.ProwJob) (*prowjobv1.ProwJob, error) {
	logrus.Debugf("updateProwJob(%s)", pj.Name)
	return c.pjc.ProwV1().ProwJobs(c.pjNamespace()).Update(pj)
}

func (c *Controller) patchProwJob(newpj *prowjobv1.ProwJob) error {
	pj, err := c.pjc.ProwV1().ProwJobs(newpj.Namespace).Get(newpj.GetName(), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting ProwJob/%s: %v", newpj.GetName(), err)
	}
	// Skip updating the resource version to avoid conflicts
	newpj.ObjectMeta.ResourceVersion = pj.ObjectMeta.ResourceVersion
	newpjData, err := json.Marshal(newpj)
	if err != nil {
		return fmt.Errorf("marshaling the new ProwJob/%s: %v", newpj.GetName(), err)
	}
	pjData, err := json.Marshal(pj)
	if err != nil {
		return fmt.Errorf("marshaling the ProwJob/%s: %v", pj.GetName(), err)
	}
	patch, err := jsonpatch.CreateMergePatch(pjData, newpjData)
	if err != nil {
		return fmt.Errorf("creating merge patch: %v", err)
	}
	if len(patch) == 0 {
		return nil
	}
	logrus.Infof("Created merge patch: %v", string(patch))
	_, err = c.pjc.ProwV1().ProwJobs(pj.Namespace).Patch(pj.Name, types.MergePatchType, patch)
	return err
}

func (c *Controller) getPipelineRun(context, namespace, name string) (*pipelinev1alpha1.PipelineRun, error) {
	p, err := c.getPipelineConfig(context)
	if err != nil {
		return nil, err
	}
	return p.Informer.Lister().PipelineRuns(namespace).Get(name)
}

func (c *Controller) getPipelineRunsWithSelector(context, namespace, selector string) ([]*pipelinev1alpha1.PipelineRun, error) {
	p, err := c.getPipelineConfig(context)
	if err != nil {
		return nil, err
	}

	label, err := labels.Parse(selector)
	if err != nil {
		return nil, fmt.Errorf("failed to parse selector %s", selector)
	}
	runs, err := p.Informer.Lister().PipelineRuns(namespace).List(label)
	if err != nil {
		return nil, fmt.Errorf("failed to list pipelineruns with label %s", label.String())
	}
	if len(runs) > 1 {
		return nil, fmt.Errorf("%s pipelineruns found with label %s, expected only 1", string(len(runs)), label.String())
	}
	if len(runs) == 0 {
		return nil, apierrors.NewNotFound(pipelinev1alpha1.Resource("pipelinerun"), label.String())
	}
	sort.Slice(runs, func(i int, j int) bool {
		return runs[i].CreationTimestamp.Before(&runs[j].CreationTimestamp)
	})
	return runs, nil
}

func (c *Controller) deletePipelineRun(context, namespace, name string) error {
	logrus.Debugf("deletePipeline(%s,%s,%s)", context, namespace, name)
	p, err := c.getPipelineConfig(context)
	if err != nil {
		return err
	}
	return p.Client.TektonV1alpha1().PipelineRuns(namespace).Delete(name, &metav1.DeleteOptions{})
}
func (c *Controller) createPipelineRun(context, namespace string, p *pipelinev1alpha1.PipelineRun) (*pipelinev1alpha1.PipelineRun, error) {
	logrus.Debugf("createPipelineRun(%s,%s,%s)", context, namespace, p.Name)
	pc, err := c.getPipelineConfig(context)
	if err != nil {
		return nil, err
	}
	return pc.Client.TektonV1alpha1().PipelineRuns(namespace).Create(p)
}

func (c *Controller) createPipelineResource(context, namespace string, pr *pipelinev1alpha1.PipelineResource) (*pipelinev1alpha1.PipelineResource, error) {
	logrus.Debugf("createPipelineResource(%s,%s,%s)", context, namespace, pr.Name)
	pc, err := c.getPipelineConfig(context)
	if err != nil {
		return nil, err
	}
	return pc.Client.TektonV1alpha1().PipelineResources(namespace).Create(pr)
}

func (c *Controller) now() metav1.Time {
	return metav1.Now()
}

func (c *Controller) pipelineID(pj prowjobv1.ProwJob) (string, error) {
	if pj.Spec.Refs == nil {
		return "", fmt.Errorf("no spec refs")
	}
	if pj.Spec.Refs.Org == "" {
		return "", fmt.Errorf("spec refs org is empty")
	}
	if pj.Spec.Refs.Repo == "" {
		return "", fmt.Errorf("spec refs repo is empty")
	}
	spec := downwardapi.NewJobSpec(pj.Spec, "", pj.Name)
	branch := getBranch(&spec)
	jobName := fmt.Sprintf("%s/%s/%s", pj.Spec.Refs.Org, pj.Spec.Refs.Repo, branch)
	return pjutil.GetBuildID(jobName, c.totURL)
}

func (c *Controller) getProwJobURL(pj prowjobv1.ProwJob) string {
	return pjutil.JobURL(c.config().Plank, pj, c.log)
}

var (
	groupVersionKind = schema.GroupVersionKind{
		Group:   prowjobv1.SchemeGroupVersion.Group,
		Version: prowjobv1.SchemeGroupVersion.Version,
		Kind:    "ProwJob",
	}
)

// reconcile ensures a tekton prowjob has a corresponding pipeline, updating the prowjob's status as the pipeline progresses.
func reconcile(c reconciler, key string) error {
	logrus.Debugf("Reconcile: %s\n", key)

	ctx, namespace, name, kind, err := fromKey(key)
	if err != nil {
		runtime.HandleError(err)
		return nil
	}

	var wantPipelineRun bool
	var havePipelineRun bool
	var reported bool
	var pj *prowjobv1.ProwJob
	var p *pipelinev1alpha1.PipelineRun
	var pr *pipelinev1alpha1.PipelineResource
	var runs []*pipelinev1alpha1.PipelineRun

	switch kind {
	case prowJob:
		pj, err = c.getProwJob(name)
		switch {
		case apierrors.IsNotFound(err):
			// Do not want pipeline
		case err != nil:
			return fmt.Errorf("get prowjob: %v", err)
		case pj.Spec.Agent != prowjobv1.TektonAgent:
			// Do not want a pipeline for this job
		case pj.Spec.Cluster != ctx:
			// Build is in wrong cluster, we do not want this build
			logrus.Warnf("%s found in context %s not %s", key, ctx, pj.Spec.Cluster)
		case pj.DeletionTimestamp == nil:
			wantPipelineRun = true
		}

		if pj == nil {
			return fmt.Errorf("no prowjob %q found", name)
		}

		//Add extra check as there is a delay for pipelinerun objects being created and we can get a watch event here
		//that updates the prowjob with the reporter status and still not have a pipelinerun which causes a duplicate
		//pipelinerun being requested
		reported = isReported(&pj.Status)

		selector := fmt.Sprintf("%s = %s", prowJobName, name)
		runs, err = c.getPipelineRunsWithSelector(ctx, namespace, selector)
		switch {
		case apierrors.IsNotFound(err):
			// Do not have a pipeline
		case err != nil:
			return fmt.Errorf("get pipelineruns %s: %v", key, err)
		}
		if len(runs) > 0 {
			havePipelineRun = true
		}
	case pipelineRun:
		p, err = c.getPipelineRun(ctx, namespace, name)
		if err != nil {
			return fmt.Errorf("no pipelinerun found with name %s: %v", name, err)
		}
		prowJobName := p.Labels[prowJobName]
		if prowJobName == "" {
			return fmt.Errorf("no prowjob name label for pipelinerun %s: %v", name, err)
		}

		pj, err = c.getProwJob(prowJobName)
		if err != nil {
			return fmt.Errorf("no matching prowjob for pipelinerun %s: %v", name, err)
		}

		selector := fmt.Sprintf("%s = %s", prowJobName, name)
		runs, err = c.getPipelineRunsWithSelector(ctx, namespace, selector)
		if err != nil {
			return fmt.Errorf("get pipelineruns %s by prow job %s: %v", key, prowJobName, err)
		}
		havePipelineRun = true

		if p.DeletionTimestamp == nil {
			wantPipelineRun = true
		}
	}

	switch {
	case !wantPipelineRun:
		if !havePipelineRun {
			if pj != nil && pj.Spec.Agent == prowjobv1.TektonAgent {
				logrus.Infof("Observed deleted: %s", key)
			}
			return nil
		}
		// Skip deleting if the job is addressed to a different cluster
		if ctx != pj.Spec.Cluster {
			return nil
		}
		// Skip deleting if the pipeline run is not created by prow
		switch v, ok := p.Labels[kube.CreatedByProw]; {
		case !ok, v != "true":
			return nil
		}
		logrus.Infof("Delete pipelinerun: %s", key)
		if err = c.deletePipelineRun(ctx, namespace, name); err != nil {
			return fmt.Errorf("delete pipelinerun: %v", err)
		}
		return nil
	case finalState(pj.Status.State):
		logrus.Infof("Observed finished: %s", key)
		return nil
	case wantPipelineRun && !havePipelineRun && !reported:
		if pj.Spec.PipelineRunSpec == nil || pj.Spec.PipelineRunSpec.PipelineRef.Name == "" {
			logrus.Infof("Create pipelinerun using external pipeline runner service: %s", key)
			pipelineRunName, err := c.requestPipelineRun(ctx, namespace, *pj)
			if err != nil {
				// Set the prow job in error state to avoid an endless loop if the pipeline request fails
				return updateProwJobState(c, pj, prowjobv1.ErrorState, err.Error())
			}
			p, err = c.getPipelineRun(ctx, namespace, pipelineRunName)
			if err != nil {
				return fmt.Errorf("finding pipeline %q: %v", pipelineRunName, err)
			}
			runs = append(runs, p)
			pj.Status.BuildID = getBuildID(p)
			pj.Status.URL = c.getProwJobURL(*pj)
		} else {
			logrus.Infof("Create pipelinerun using embedded spec: %s", key)
			id, err := c.pipelineID(*pj)
			if err != nil {
				return fmt.Errorf("failed to get pipeline id: %v", err)
			}
			pj.Status.BuildID = id
			pj.Status.URL = c.getProwJobURL(*pj)
			pr = makePipelineGitResource(*pj)
			logrus.Infof("Create pipeline git resource: %s", key)
			if pr, err = c.createPipelineResource(ctx, namespace, pr); err != nil {
				return fmt.Errorf("create PipelineResource: %v", err)
			}
			newp, err := makePipelineRun(*pj, id, pr)
			if err != nil {
				return fmt.Errorf("make PipelineRun: %v", err)
			}
			logrus.Infof("Create pipelinerun%s", key)
			p, err = c.createPipelineRun(ctx, namespace, newp)
			if err != nil {
				return fmt.Errorf("create PipelineRun: %v", err)
			}
			runs = append(runs, p)
		}
	}

	if len(runs) == 0 {
		return fmt.Errorf("no pipelinerun found or created for %q, wantPipelineRun was %v", key, wantPipelineRun)
	}
	wantState, wantMsg := prowJobStatus(runs)
	return updateProwJobState(c, pj, wantState, wantMsg)
}

func updateProwJobState(c reconciler, pj *prowjobv1.ProwJob, state prowjobv1.ProwJobState, msg string) error {
	haveState := pj.Status.State
	haveMsg := pj.Status.Description
	if haveState != state || haveMsg != msg {
		npj := pj.DeepCopy()
		if npj.Status.StartTime.IsZero() {
			npj.Status.StartTime = c.now()
		}
		if npj.Status.CompletionTime.IsZero() && finalState(state) {
			now := c.now()
			npj.Status.CompletionTime = &now
		}
		npj.Status.State = state
		npj.Status.Description = msg
		logrus.Infof("Update ProwJob/%s: %s - %s [ %s ]", pj.GetName(), haveState, state, msg)
		if err := c.patchProwJob(npj); err != nil {
			return fmt.Errorf("update prow status: %v", err)
		}
	}
	return nil
}

// isReported check if a prow job was already reported
func isReported(status *prowjobv1.ProwJobStatus) bool {
	if status != nil && status.PrevReportStates != nil && (status.PrevReportStates["github-reporter"] == prowjobv1.PendingState) {
		return true
	}
	return false
}

// finalState returns true if the prowjob has already finished
func finalState(status prowjobv1.ProwJobState) bool {
	switch status {
	case "", prowjobv1.PendingState, prowjobv1.TriggeredState:
		return false
	}
	return true
}

// description computes the ProwJobStatus description for this condition or falling back to a default if none is provided.
func description(cond duckv1alpha1.Condition, fallback string) string {
	switch {
	case cond.Message != "":
		return cond.Message
	case cond.Reason != "":
		return cond.Reason
	}
	return fallback
}

const (
	descScheduling       = "scheduling"
	descInitializing     = "initializing"
	descRunning          = "running"
	descSucceeded        = "succeeded"
	descFailed           = "failed"
	descUnknown          = "unknown status"
	descMissingCondition = "missing end condition"
)

// prowJobStatus returns the worst state of any of the PipelineRuns passed to it
func prowJobStatus(runs []*pipelinev1alpha1.PipelineRun) (prowjobv1.ProwJobState, string) {
	var worstState *prowjobv1.ProwJobState
	worstDesc := ""

	for _, pr := range runs {
		runState, runDesc := prowJobStatusForSinglePipelineRun(pr.Status)
		if worstState == nil {
			worstState = &runState
			worstDesc = runDesc
		} else if worstState != &runState {
			// Always overwrite earlier successes.
			if *worstState == prowjobv1.SuccessState {
				worstState = &runState
				worstDesc = runDesc
			}
		}
	}

	return *worstState, worstDesc
}

// prowJobStatusForSinglePipelineRun returns the desired state and description based on the pipeline status
func prowJobStatusForSinglePipelineRun(ps pipelinev1alpha1.PipelineRunStatus) (prowjobv1.ProwJobState, string) {
	pcond := ps.GetCondition(duckv1alpha1.ConditionSucceeded)
	if pcond == nil {
		return prowjobv1.TriggeredState, descScheduling
	}
	cond := *pcond
	switch {
	case cond.Status == untypedcorev1.ConditionTrue:
		return prowjobv1.SuccessState, description(cond, descSucceeded)
	case cond.Status == untypedcorev1.ConditionFalse:
		return prowjobv1.FailureState, description(cond, descFailed)
	case cond.Status == untypedcorev1.ConditionUnknown:
		return prowjobv1.PendingState, description(cond, descRunning)
	}

	logrus.Warnf("Unknown condition %#v", cond)
	return prowjobv1.ErrorState, description(cond, descUnknown) // shouldn't happen
}

// ownerRef builds an owner reference from prow job
func ownerRef(pj prowjobv1.ProwJob) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: pj.APIVersion,
		Kind:       pj.Kind,
		Name:       pj.Name,
		UID:        pj.UID,
	}
}

// pipelineMeta builds the pipeline metadata from prow job definition
func pipelineMeta(pj prowjobv1.ProwJob) metav1.ObjectMeta {
	podLabels, annotations := decorate.LabelsAndAnnotationsForJob(pj)
	return metav1.ObjectMeta{
		Annotations:     annotations,
		Name:            pj.Name,
		Namespace:       pj.Spec.Namespace,
		Labels:          podLabels,
		OwnerReferences: []metav1.OwnerReference{ownerRef(pj)},
	}
}

// defaultEnv adds the map of environment variables to the container, except keys already defined.
func defaultEnv(c *untypedcorev1.Container, rawEnv map[string]string) {
	keys := sets.String{}
	for _, arg := range c.Env {
		keys.Insert(arg.Name)
	}
	for _, k := range sets.StringKeySet(rawEnv).List() { // deterministic ordering
		if keys.Has(k) {
			continue
		}
		c.Env = append(c.Env, untypedcorev1.EnvVar{Name: k, Value: rawEnv[k]})
	}
}

// makePipelineGitResource creates a pipeline git resource from prow job
func makePipelineGitResource(pj prowjobv1.ProwJob) *pipelinev1alpha1.PipelineResource {
	sourceURL := ""
	revision := ""
	if pj.Spec.Refs != nil {
		sourceURL = pj.Spec.Refs.CloneURI
		if len(pj.Spec.Refs.Pulls) > 0 {
			revision = pj.Spec.Refs.Pulls[0].SHA
		} else {
			revision = pj.Spec.Refs.BaseSHA
		}
	}
	pr := pipelinev1alpha1.PipelineResource{
		ObjectMeta: pipelineMeta(pj),
		Spec: pipelinev1alpha1.PipelineResourceSpec{
			Type: pipelinev1alpha1.PipelineResourceTypeGit,
			Params: []pipelinev1alpha1.Param{
				{
					Name:  "source",
					Value: sourceURL,
				},
				{
					Name:  "revision",
					Value: revision,
				},
			},
		},
	}
	return &pr
}

func makePipelineRun(pj prowjobv1.ProwJob, buildID string, pr *pipelinev1alpha1.PipelineResource) (*pipelinev1alpha1.PipelineRun, error) {
	return makePipelineRunWithPrefix(pj, buildID, pr, "")
}

// makePipelineRunWithPrefix creates a PipelineRun from a prow job using the PipelineRunSpec defined in the prow job
func makePipelineRunWithPrefix(pj prowjobv1.ProwJob, buildID string, pr *pipelinev1alpha1.PipelineResource, prefix string) (*pipelinev1alpha1.PipelineRun, error) {
	if pj.Spec.PipelineRunSpec == nil {
		return nil, errors.New("no PipelineRunSpec defined")
	}
	name := pr.Name
	if prefix != "" {
		name = prefix + "-" + name
	}
	p := pipelinev1alpha1.PipelineRun{
		ObjectMeta: pipelineMeta(pj),
		Spec:       *pj.Spec.PipelineRunSpec.DeepCopy(),
	}
	p.Spec.Params = append(p.Spec.Params, pipelinev1alpha1.Param{
		Name:  "build_id",
		Value: buildID,
	})
	rb := pipelinev1alpha1.PipelineResourceBinding{
		Name: name,
		ResourceRef: pipelinev1alpha1.PipelineResourceRef{
			Name:       pr.Name,
			APIVersion: pr.APIVersion,
		},
	}
	p.Spec.Resources = append(p.Spec.Resources, rb)

	return &p, nil
}

// requestPipelineRun sends a request to an external service to start the pipeline
func (c *Controller) requestPipelineRun(context, namespace string, pj prowjobv1.ProwJob) (string, error) {
	pipelineURL, err := url.Parse(c.pipelineRunnerURL())
	if err != nil {
		return "", fmt.Errorf("invalid pipelinerunner url: %v", err)
	}

	labels := map[string]string{}
	labels[prowJobName] = pj.Name
	labels[kube.CreatedByProw] = "true"
	payload := PipelineRunRequest{
		Labels:      labels,
		ProwJobSpec: pj.Spec,
	}
	jsonValue, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling the pipeline run request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, pipelineURL.String(), bytes.NewBuffer(jsonValue))
	if err != nil {
		return "", fmt.Errorf("building the pipeline run request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := http.Client{Timeout: maxPipelineRunRequestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting pipeline run: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errorMessage := "unexpected response from pipeline runner service"
		respData, err1 := ioutil.ReadAll(resp.Body)
		if err1 != nil {
			return "", fmt.Errorf("%s: %v", errorMessage, resp.Status)
		}
		return "", fmt.Errorf("%s: %v, %s", errorMessage, resp.Status, string(respData))
	}

	respData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response from pipeline runner %v: %v", resp, err)
	}
	responses := PipelineRunResponse{}
	err = json.Unmarshal(respData, &responses)
	if err != nil {
		return "", fmt.Errorf("unmarshalling response %v", err)
	}

	for _, resource := range responses.Resources {
		if resource.Kind == pipelineRun {
			return resource.Name, nil
		}
	}

	return "", fmt.Errorf("no PipelineRun object found returned by pipeline runner")
}

func getBuildID(pr *pipelinev1alpha1.PipelineRun) string {
	for _, param := range pr.Spec.Params {
		if param.Name == "build_id" {
			return param.Value
		}
	}
	// use a default build number if the real build number cannot be parsed
	return "1"
}

// getBranch returns the source code branch
func getBranch(spec *downwardapi.JobSpec) string {
	branch := spec.Refs.BaseRef
	if spec.Type == prowjobv1.PostsubmitJob || spec.Type == prowjobv1.BatchJob {
		return branch
	}
	if len(spec.Refs.Pulls) > 0 {
		branch = fmt.Sprintf("PR-%v", spec.Refs.Pulls[0].Number)
	}
	return branch
}
