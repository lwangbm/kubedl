package apiserver

import (
	"context"
	"fmt"
	v1 "github.com/alibaba/kubedl/apis/training/v1alpha1"
	"math"
	"sort"
	"strconv"
	"strings"

	clientmgr "github.com/alibaba/kubedl/console/backend/pkg/client"
	apiv1 "github.com/alibaba/kubedl/pkg/job_controller/api/v1"
	"github.com/alibaba/kubedl/pkg/storage/backends"
	"github.com/alibaba/kubedl/pkg/storage/dmo"
	"github.com/alibaba/kubedl/pkg/storage/dmo/converters"
	"github.com/alibaba/kubedl/pkg/util/workloadgate"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NewAPIServerObjectBackend() backends.ObjectStorageBackend {
	return &apiServerBackend{clientmgr.GetCtrlClient()}
}

var (
	_        backends.ObjectStorageBackend = &apiServerBackend{}
	allKinds []string
)

type apiServerBackend struct {
	client client.Client
}

func (a *apiServerBackend) Initialize() error {
	fn := func(obj runtime.Object) []string {
		meta, err := apimeta.Accessor(obj)
		if err != nil {
			return []string{}
		}
		return []string{meta.GetName()}
	}
	for _, kind := range []string{v1.TFJobKind, v1.PyTorchJobKind, v1.XDLJobKind, v1.XGBoostJobKind} {
		job := initJobWithKind(kind)
		_, enabled := workloadgate.IsWorkloadEnable(job, clientmgr.GetScheme())
		if !enabled {
			continue
		}
		allKinds = append(allKinds, kind)
		if err := clientmgr.IndexField(job, "metadata.name", fn); err != nil {
			return err
		}
	}
	return nil
}

func (a *apiServerBackend) Close() error {
	return nil
}

func (a *apiServerBackend) Name() string {
	return "apiserver"
}

func (a *apiServerBackend) SavePod(pod *corev1.Pod, defaultContainerName, region string) error {
	return nil
}

func (a *apiServerBackend) ListPods(ns, name, jobID string) ([]*dmo.Pod, error) {
	pods := corev1.PodList{}
	err := a.client.List(context.Background(), &pods, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{apiv1.JobNameLabel: name}),
		Namespace:     ns,
	})
	if err != nil {
		return nil, err
	}
	dmoPods := make([]*dmo.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		dmoPod, err := converters.ConvertPodToDMOPod(&pods.Items[i], "", "")
		if err != nil {
			return nil, err
		}
		dmoPods = append(dmoPods, dmoPod)
	}
	if len(dmoPods) > 0 {
		// Order by create timestamp.
		sort.SliceStable(dmoPods, func(i, j int) bool {
			if dmoPods[i].ReplicaType != dmoPods[j].ReplicaType {
				return dmoPods[i].ReplicaType < dmoPods[j].ReplicaType
			}
			is := strings.Split(dmoPods[i].Name, "-")
			if len(is) <= 0 {
				return false
			}
			ii, err := strconv.Atoi(is[len(is)-1])
			if err != nil {
				return false
			}
			js := strings.Split(dmoPods[j].Name, "-")
			if len(js) <= 0 {
				return true
			}
			ji, err := strconv.Atoi(js[len(js)-1])
			if err != nil {
				return true
			}
			if ii != ji {
				return ii < ji
			}
			return dmoPods[i].GmtCreated.Before(dmoPods[j].GmtCreated)
		})
	}
	return dmoPods, nil
}

func (a *apiServerBackend) StopPod(ns, name, podID string) error {
	pod := corev1.Pod{}
	err := a.client.Get(context.Background(), types.NamespacedName{
		Namespace: ns,
		Name:      name,
	}, &pod)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return a.client.Delete(context.Background(), &pod)
}

func (a *apiServerBackend) SaveJob(job metav1.Object, kind string, specs map[apiv1.ReplicaType]*apiv1.ReplicaSpec, jobStatus *apiv1.JobStatus, region string) error {
	return nil
}

func (a *apiServerBackend) GetJob(ns, name, jobID, kind, region string) (*dmo.Job, error) {
	job := initJobWithKind(kind)
	getter := initJobPropertiesWithKind(kind)
	err := a.client.Get(context.Background(), types.NamespacedName{
		Namespace: ns,
		Name:      name,
	}, job)
	if err != nil {
		return nil, err
	}
	metaObj, specs, jobStatus := getter(job)
	/*
		enableGPUTopo := runPolicy.GPUTopologyPolicy != nil && runPolicy.GPUTopologyPolicy.IsTopologyAware
	*/
	dmoJob, err := converters.ConvertJobToDMOJob(metaObj, kind, specs, jobStatus, region)
	if err != nil {
		return nil, err
	}
	return dmoJob, nil
}

func (a *apiServerBackend) ListJobs(query *backends.Query) ([]*dmo.Job, error) {
	klog.Infof("list jobs with query: %+v", query)
	// List job lists for each job kind.
	var (
		options  client.ListOptions
		filters  []func(job *dmo.Job) bool
		dmoJobs  []*dmo.Job
		jobTypes []string
	)
	if query.Namespace != "" {
		options.Namespace = query.Namespace
	}
	if query.StartTime.IsZero() || query.EndTime.IsZero() {
		return nil, fmt.Errorf("StartTime EndTime should not be empty")
	}
	filters = append(filters, func(job *dmo.Job) bool {
		if job.GmtCreated.Before(query.StartTime) {
			if job.GmtJobFinished == nil || job.GmtJobFinished.IsZero() {
				return true
			}
			if job.GmtJobFinished != nil && job.GmtJobFinished.After(query.StartTime) {
				return true
			}
			return false
		}
		if job.GmtCreated.Before(query.EndTime) {
			return true
		}
		return false
	})
	if query.Status != "" {
		filters = append(filters, func(job *dmo.Job) bool {
			return strings.ToLower(string(job.Status)) ==
				strings.ToLower(string(query.Status))
		})
	}
	if query.Type != "" {
		if !stringSliceContains(query.Type, allKinds) {
			return nil, fmt.Errorf("unsupported job kind [%s]", query.Type)
		}
		jobTypes = []string{query.Type}
	} else {
		jobTypes = allKinds
	}
	for _, kind := range jobTypes {
		jobs, err := a.listJobsWithKind(kind, query.Name, query.Region, options, filters...)
		if err != nil {
			return nil, err
		}
		dmoJobs = append(dmoJobs, jobs...)
	}

	if len(dmoJobs) > 1 {
		// Order by create timestamp.
		sort.SliceStable(dmoJobs, func(i, j int) bool {
			if dmoJobs[i].GmtCreated.Equal(dmoJobs[j].GmtCreated) {
				return dmoJobs[i].Name < dmoJobs[j].Name
			}
			return dmoJobs[i].GmtCreated.After(dmoJobs[j].GmtCreated)
		})
	}

	if query.Pagination != nil && len(dmoJobs) > 1 {
		query.Pagination.Count = len(dmoJobs)
		count := query.Pagination.Count
		pageNum := query.Pagination.PageNum
		pageSize := query.Pagination.PageSize
		startIdx := pageSize * (pageNum - 1)
		if startIdx < 0 {
			startIdx = 0
		}
		if startIdx > len(dmoJobs)-1 {
			startIdx = len(dmoJobs) - 1
		}
		endIdx := len(dmoJobs)
		if count > 0 {
			endIdx = int(math.Min(float64(startIdx+pageSize), float64(endIdx)))
		}
		klog.Infof("list jobs with pagination, start index: %d, end index: %d", startIdx, endIdx)
		dmoJobs = dmoJobs[startIdx:endIdx]
	}
	return dmoJobs, nil
}

func (a *apiServerBackend) StopJob(ns, name, jobID, kind, region string) error {
	job := initJobWithKind(kind)
	err := a.client.Get(context.Background(), types.NamespacedName{
		Namespace: ns,
		Name:      name,
	}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return a.client.Delete(context.Background(), job)
}

func (a *apiServerBackend) DeleteJob(ns, name, jobID, kind, region string) error {
	return a.StopJob(ns, name, jobID, kind, region)
}

func (a *apiServerBackend) listJobsWithKind(kind string, nameLike, region string, options client.ListOptions, filters ...func(*dmo.Job) bool) ([]*dmo.Job, error) {
	list, lister := initJobListWithKind(kind)
	if err := a.client.List(context.Background(), list, &options); err != nil {
		return nil, err
	}
	jobs := lister(list)
	getter := initJobPropertiesWithKind(kind)
	dmoJobs := make([]*dmo.Job, 0, len(jobs))
	for _, job := range jobs {
		metaObj, specs, jobStatus := getter(job)
		if nameLike != "" && !strings.Contains(metaObj.GetName(), nameLike) {
			continue
		}
		/*
			enableGPUTopo := runPolicy.GPUTopologyPolicy != nil && runPolicy.GPUTopologyPolicy.IsTopologyAware
		*/
		dmoJob, err := converters.ConvertJobToDMOJob(metaObj, kind, specs, jobStatus, region)
		if err != nil {
			return nil, err
		}
		skip := false
		for _, filter := range filters {
			if !filter(dmoJob) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		dmoJobs = append(dmoJobs, dmoJob)
	}
	return dmoJobs, nil
}

func initJobWithKind(kind string) (job runtime.Object) {
	switch kind {
	case v1.TFJobKind:
		job = &v1.TFJob{}
	case v1.PyTorchJobKind:
		job = &v1.PyTorchJob{}
	case v1.XDLJobKind:
		job = &v1.XDLJob{}
	case v1.XGBoostJobKind:
		job = &v1.XGBoostJob{}
	}
	return
}

type jobPropertiesGetter func(obj runtime.Object) (metav1.Object, map[apiv1.ReplicaType]*apiv1.ReplicaSpec, *apiv1.JobStatus)

func initJobPropertiesWithKind(kind string) (getter jobPropertiesGetter) {
	switch kind {
	case v1.TFJobKind:
		getter = func(obj runtime.Object) (metav1.Object, map[apiv1.ReplicaType]*apiv1.ReplicaSpec, *apiv1.JobStatus) {
			tfJob := obj.(*v1.TFJob)
			return tfJob, tfJob.Spec.TFReplicaSpecs, &tfJob.Status
		}
	case v1.PyTorchJobKind:
		getter = func(obj runtime.Object) (metav1.Object, map[apiv1.ReplicaType]*apiv1.ReplicaSpec, *apiv1.JobStatus) {
			pytorchJob := obj.(*v1.PyTorchJob)
			return pytorchJob, pytorchJob.Spec.PyTorchReplicaSpecs, &pytorchJob.Status
		}
	case v1.XDLJobKind:
		getter = func(obj runtime.Object) (metav1.Object, map[apiv1.ReplicaType]*apiv1.ReplicaSpec, *apiv1.JobStatus) {
			xdlJob := obj.(*v1.XDLJob)
			return xdlJob, xdlJob.Spec.XDLReplicaSpecs, &xdlJob.Status
		}
	case v1.XGBoostJobKind:
		getter = func(obj runtime.Object) (metav1.Object, map[apiv1.ReplicaType]*apiv1.ReplicaSpec, *apiv1.JobStatus) {
			xgboostJob := obj.(*v1.XGBoostJob)
			return xgboostJob, xgboostJob.Spec.XGBReplicaSpecs, &xgboostJob.Status.JobStatus
		}
	}
	return
}

type jobLister func(list runtime.Object) []runtime.Object

func initJobListWithKind(kind string) (list runtime.Object, lister jobLister) {
	switch kind {
	case v1.TFJobKind:
		list = &v1.TFJobList{}
		lister = func(list runtime.Object) []runtime.Object {
			tfList := list.(*v1.TFJobList)
			jobs := make([]runtime.Object, 0, len(tfList.Items))
			for i := range tfList.Items {
				jobs = append(jobs, &tfList.Items[i])
			}
			return jobs
		}
	case v1.PyTorchJobKind:
		list = &v1.PyTorchJobList{}
		lister = func(list runtime.Object) []runtime.Object {
			pytorchList := list.(*v1.PyTorchJobList)
			jobs := make([]runtime.Object, 0, len(pytorchList.Items))
			for i := range pytorchList.Items {
				jobs = append(jobs, &pytorchList.Items[i])
			}
			return jobs
		}
	case v1.XDLJobKind:
		list = &v1.XDLJobList{}
		lister = func(list runtime.Object) []runtime.Object {
			xdlList := list.(*v1.XDLJobList)
			jobs := make([]runtime.Object, 0, len(xdlList.Items))
			for i := range xdlList.Items {
				jobs = append(jobs, &xdlList.Items[i])
			}
			return jobs
		}
	case v1.XGBoostJobKind:
		list = &v1.XGBoostJobList{}
		lister = func(list runtime.Object) []runtime.Object {
			xgboostList := list.(*v1.XGBoostJobList)
			jobs := make([]runtime.Object, 0, len(xgboostList.Items))
			for i := range xgboostList.Items {
				jobs = append(jobs, &xgboostList.Items[i])
			}
			return jobs
		}
	}
	return
}

func stringSliceContains(val string, slice []string) bool {
	for _, v := range slice {
		if v == val {
			return true
		}
	}
	return false
}
