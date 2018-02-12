package server

import (
	"bufio"
	"bytes"
	goerr "errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/auth"
	"github.com/pachyderm/pachyderm/src/client/limit"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/grpcutil"
	"github.com/pachyderm/pachyderm/src/client/pkg/uuid"
	"github.com/pachyderm/pachyderm/src/client/pps"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/hashtree"
	"github.com/pachyderm/pachyderm/src/server/pkg/log"
	"github.com/pachyderm/pachyderm/src/server/pkg/metrics"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsconsts"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsdb"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsutil"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"
	ppsserver "github.com/pachyderm/pachyderm/src/server/pps"
	"github.com/pachyderm/pachyderm/src/server/pps/server/githook"
	workerpkg "github.com/pachyderm/pachyderm/src/server/worker"
	"github.com/robfig/cron"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/types"
	logrus "github.com/sirupsen/logrus"
	"golang.org/x/net/context"

	"golang.org/x/sync/errgroup"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kube "k8s.io/client-go/kubernetes"
)

const (
	// MaxPodsPerChunk is the maximum number of pods we can schedule for each
	// chunk in case of failures.
	MaxPodsPerChunk = 3
	// DefaultUserImage is the image used for jobs when the user does not specify
	// an image.
	DefaultUserImage = "ubuntu:16.04"
)

var (
	trueVal = true
	zeroVal = int64(0)
	suite   = "pachyderm"
)

func newErrJobNotFound(job string) error {
	return fmt.Errorf("job %v not found", job)
}

func newErrPipelineNotFound(pipeline string) error {
	return fmt.Errorf("pipeline %v not found", pipeline)
}

func newErrPipelineExists(pipeline string) error {
	return fmt.Errorf("pipeline %v already exists", pipeline)
}

type errEmptyInput struct {
	error
}

func newErrEmptyInput(commitID string) *errEmptyInput {
	return &errEmptyInput{
		error: fmt.Errorf("job was not started due to empty input at commit %v", commitID),
	}
}

type errGithookServiceNotFound struct {
	error
}

func newErrParentInputsMismatch(parent string) error {
	return fmt.Errorf("job does not have the same set of inputs as its parent %v", parent)
}

type ctxAndCancel struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type apiServer struct {
	log.Logger
	etcdPrefix            string
	hasher                *ppsserver.Hasher
	address               string
	etcdClient            *etcd.Client
	kubeClient            *kube.Clientset
	pachClient            *client.APIClient
	pachClientOnce        sync.Once
	namespace             string
	workerImage           string
	workerSidecarImage    string
	workerImagePullPolicy string
	storageRoot           string
	storageBackend        string
	storageHostPath       string
	iamRole               string
	imagePullSecret       string
	reporter              *metrics.Reporter
	// collections
	pipelines col.Collection
	jobs      col.Collection
}

func merge(from, to map[string]bool) {
	for s := range from {
		to[s] = true
	}
}

func validateNames(names map[string]bool, input *pps.Input) error {
	switch {
	case input.Atom != nil:
		if names[input.Atom.Name] {
			return fmt.Errorf("name %s was used more than once", input.Atom.Name)
		}
		names[input.Atom.Name] = true
	case input.Cron != nil:
		if names[input.Cron.Name] {
			return fmt.Errorf("name %s was used more than once", input.Cron.Name)
		}
		names[input.Cron.Name] = true
	case input.Union != nil:
		for _, input := range input.Union {
			namesCopy := make(map[string]bool)
			merge(names, namesCopy)
			if err := validateNames(namesCopy, input); err != nil {
				return err
			}
			// we defer this because subinputs of a union input are allowed to
			// have conflicting names but other inputs that are, for example,
			// crossed with this union cannot conflict with any of the names it
			// might present
			defer merge(namesCopy, names)
		}
	case input.Cross != nil:
		for _, input := range input.Cross {
			if err := validateNames(names, input); err != nil {
				return err
			}
		}
	case input.Git != nil:
		if names[input.Git.Name] == true {
			return fmt.Errorf("name %s was used more than once", input.Git.Name)
		}
		names[input.Git.Name] = true
	}
	return nil
}

func (a *apiServer) validateInput(pachClient *client.APIClient, pipelineName string, input *pps.Input, job bool) error {
	if err := validateNames(make(map[string]bool), input); err != nil {
		return err
	}
	var result error
	pps.VisitInput(input, func(input *pps.Input) {
		if err := func() error {
			set := false
			if input.Atom != nil {
				set = true
				switch {
				case len(input.Atom.Name) == 0:
					return fmt.Errorf("input must specify a name")
				case input.Atom.Name == "out":
					return fmt.Errorf("input cannot be named \"out\", as pachyderm " +
						"already creates /pfs/out to collect job output")
				case input.Atom.Repo == "":
					return fmt.Errorf("input must specify a repo")
				case input.Atom.Branch == "" && !job:
					return fmt.Errorf("input must specify a branch")
				case len(input.Atom.Glob) == 0:
					return fmt.Errorf("input must specify a glob")
				}
				// Note that input.Atom.Commit is empty if a) this is a job b) one of
				// the job pipeline's input branches has no commits yet
				if job && input.Atom.Commit != "" {
					// for jobs we check that the input commit exists
					if _, err := pachClient.InspectCommit(input.Atom.Repo, input.Atom.Commit); err != nil {
						return err
					}
				} else {
					// for pipelines we only check that the repo exists
					if _, err := pachClient.InspectRepo(input.Atom.Repo); err != nil {
						return err
					}
				}
			}
			if input.Cross != nil {
				if set {
					return fmt.Errorf("multiple input types set")
				}
				set = true
			}
			if input.Union != nil {
				if set {
					return fmt.Errorf("multiple input types set")
				}
				set = true
			}
			if input.Cron != nil {
				if set {
					return fmt.Errorf("multiple input types set")
				}
				set = true
				if _, err := cron.Parse(input.Cron.Spec); err != nil {
					return err
				}
			}
			if input.Git != nil {
				if set {
					return fmt.Errorf("multiple input types set")
				}
				set = true
				if err := pps.ValidateGitCloneURL(input.Git.URL); err != nil {
					return err
				}
			}
			if !set {
				return fmt.Errorf("no input set")
			}
			return nil
		}(); err != nil && result == nil {
			result = err
		}
	})
	return result
}

func validateTransform(transform *pps.Transform) error {
	if len(transform.Cmd) == 0 {
		return fmt.Errorf("no cmd set")
	}
	return nil
}

func (a *apiServer) validateJob(pachClient *client.APIClient, jobInfo *pps.JobInfo) error {
	if err := validateTransform(jobInfo.Transform); err != nil {
		return err
	}
	return a.validateInput(pachClient, jobInfo.Pipeline.Name, jobInfo.Input, true)
}

func (a *apiServer) validateKube() {
	errors := false
	_, err := a.kubeClient.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		errors = true
		logrus.Errorf("unable to access kubernetes nodeslist, Pachyderm will continue to work but it will not be possible to use COEFFICIENT parallelism. error: %v", err)
	}
	_, err = a.kubeClient.CoreV1().Pods(a.namespace).Watch(metav1.ListOptions{Watch: true})
	if err != nil {
		errors = true
		logrus.Errorf("unable to access kubernetes pods, Pachyderm will continue to work but certain pipeline errors will result in pipelines being stuck indefinitely in \"starting\" state. error: %v", err)
	}
	pods, err := a.rcPods("pachd")
	if err != nil {
		errors = true
		logrus.Errorf("unable to access kubernetes pods, Pachyderm will continue to work but get-logs will not work. error: %v", err)
	} else {
		for _, pod := range pods {
			_, err = a.kubeClient.CoreV1().Pods(a.namespace).GetLogs(
				pod.ObjectMeta.Name, &v1.PodLogOptions{
					Container: "pachd",
				}).Timeout(10 * time.Second).Do().Raw()
			if err != nil {
				errors = true
				logrus.Errorf("unable to access kubernetes logs, Pachyderm will continue to work but get-logs will not work. error: %v", err)
			}
			break
		}
	}
	name := uuid.NewWithoutDashes()
	labels := map[string]string{"app": name}
	rc := &v1.ReplicationController{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ReplicationController",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: v1.ReplicationControllerSpec{
			Selector: labels,
			Replicas: new(int32),
			Template: &v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:   name,
					Labels: labels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:    "name",
							Image:   DefaultUserImage,
							Command: []string{"true"},
						},
					},
				},
			},
		},
	}
	if _, err := a.kubeClient.CoreV1().ReplicationControllers(a.namespace).Create(rc); err != nil {
		if err != nil {
			errors = true
			logrus.Errorf("unable to create kubernetes replication controllers, Pachyderm will not function properly until this is fixed. error: %v", err)
		}
	}
	if err := a.kubeClient.CoreV1().ReplicationControllers(a.namespace).Delete(name, nil); err != nil {
		if err != nil {
			errors = true
			logrus.Errorf("unable to delete kubernetes replication controllers, Pachyderm function properly but pipeline cleanup will not work. error: %v", err)
		}
	}
	if !errors {
		logrus.Infof("validating kubernetes access returned no errors")
	}
}

func (a *apiServer) CreateJob(ctx context.Context, request *pps.CreateJobRequest) (response *pps.Job, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)
	ctx = pachClient.Ctx() // pachClient will propagate auth info

	job := &pps.Job{uuid.NewWithoutDashes()}
	_, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		jobPtr := &pps.EtcdJobInfo{
			Job:          job,
			OutputCommit: request.OutputCommit,
			Pipeline:     request.Pipeline,
			Stats:        &pps.ProcessStats{},
		}
		return a.updateJobState(stm, jobPtr, pps.JobState_JOB_STARTING)
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (a *apiServer) InspectJob(ctx context.Context, request *pps.InspectJobRequest) (response *pps.JobInfo, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)

	jobs := a.jobs.ReadOnly(ctx)

	if request.BlockState {
		watcher, err := jobs.WatchOne(request.Job.ID)
		if err != nil {
			return nil, err
		}
		defer watcher.Close()

		for {
			ev, ok := <-watcher.Watch()
			if !ok {
				return nil, fmt.Errorf("the stream for job updates closed unexpectedly")
			}
			switch ev.Type {
			case watch.EventError:
				return nil, ev.Err
			case watch.EventDelete:
				return nil, fmt.Errorf("job %s was deleted", request.Job.ID)
			case watch.EventPut:
				var jobID string
				jobPtr := &pps.EtcdJobInfo{}
				if err := ev.Unmarshal(&jobID, jobPtr); err != nil {
					return nil, err
				}
				if ppsutil.IsTerminal(jobPtr.State) {
					return a.jobInfoFromPtr(pachClient, jobPtr)
				}
			}
		}
	}

	jobPtr := &pps.EtcdJobInfo{}
	if err := jobs.Get(request.Job.ID, jobPtr); err != nil {
		return nil, err
	}
	jobInfo, err := a.jobInfoFromPtr(pachClient, jobPtr)
	if err != nil {
		return nil, err
	}
	// If the job is running we fill in WorkerStatus field, otherwise we just
	// return the jobInfo.
	if jobInfo.State != pps.JobState_JOB_RUNNING {
		return jobInfo, nil
	}
	workerPoolID := ppsutil.PipelineRcName(jobInfo.Pipeline.Name, jobInfo.PipelineVersion)
	workerStatus, err := status(ctx, workerPoolID, a.etcdClient, a.etcdPrefix)
	if err != nil {
		logrus.Errorf("failed to get worker status with err: %s", err.Error())
	} else {
		// It's possible that the workers might be working on datums for other
		// jobs, we omit those since they're not part of the status for this
		// job.
		for _, status := range workerStatus {
			if status.JobID == jobInfo.Job.ID {
				jobInfo.WorkerStatus = append(jobInfo.WorkerStatus, status)
			}
		}
	}
	return jobInfo, nil
}

// listJob is the internal implementation of ListJob shared between ListJob and
// ListJobStream. When ListJob is removed, this should be inlined into
// ListJobStream.
func (a *apiServer) listJob(pachClient *client.APIClient, pipeline *pps.Pipeline, outputCommit *pfs.Commit, inputCommits []*pfs.Commit) ([]*pps.JobInfo, error) {
	var err error
	if outputCommit != nil {
		outputCommit, err = a.resolveCommit(pachClient, outputCommit)
		if err != nil {
			return nil, err
		}
	}
	for i, inputCommit := range inputCommits {
		inputCommits[i], err = a.resolveCommit(pachClient, inputCommit)
		if err != nil {
			return nil, err
		}
	}
	jobs := a.jobs.ReadOnly(pachClient.Ctx())
	var iter col.Iterator
	if pipeline != nil {
		iter, err = jobs.GetByIndex(ppsdb.JobsPipelineIndex, pipeline)
	} else if outputCommit != nil {
		iter, err = jobs.GetByIndex(ppsdb.JobsOutputIndex, outputCommit)
	} else {
		iter, err = jobs.List()
	}
	if err != nil {
		return nil, err
	}

	var jobInfos []*pps.JobInfo
JobsLoop:
	for {
		var jobID string
		var jobPtr pps.EtcdJobInfo
		ok, err := iter.Next(&jobID, &jobPtr)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		jobInfo, err := a.jobInfoFromPtr(pachClient, &jobPtr)
		if err != nil {
			return nil, err
		}
		if len(inputCommits) > 0 {
			found := make([]bool, len(inputCommits))
			pps.VisitInput(jobInfo.Input, func(in *pps.Input) {
				if in.Atom != nil {
					for i, inputCommit := range inputCommits {
						if in.Atom.Commit == inputCommit.ID {
							found[i] = true
						}
					}
				}
			})
			for _, found := range found {
				if !found {
					continue JobsLoop
				}
			}
		}
		jobInfos = append(jobInfos, jobInfo)
	}
	return jobInfos, nil
}

func (a *apiServer) jobInfoFromPtr(pachClient *client.APIClient, jobPtr *pps.EtcdJobInfo) (*pps.JobInfo, error) {
	// TODO accept pachClient argument
	result := &pps.JobInfo{
		Job:           jobPtr.Job,
		Pipeline:      jobPtr.Pipeline,
		OutputCommit:  jobPtr.OutputCommit,
		Restart:       jobPtr.Restart,
		DataProcessed: jobPtr.DataProcessed,
		DataSkipped:   jobPtr.DataSkipped,
		DataTotal:     jobPtr.DataTotal,
		DataFailed:    jobPtr.DataFailed,
		Stats:         jobPtr.Stats,
		StatsCommit:   jobPtr.StatsCommit,
		State:         jobPtr.State,
		Reason:        jobPtr.Reason,
	}
	commitInfo, err := pachClient.InspectCommit(jobPtr.OutputCommit.Repo.Name, jobPtr.OutputCommit.ID)
	if err != nil {
		return nil, err
	}
	result.Started = commitInfo.Started
	result.Finished = commitInfo.Finished
	var specCommit *pfs.Commit
	for i, provCommit := range commitInfo.Provenance {
		provBranch := commitInfo.BranchProvenance[i]
		if provBranch.Repo.Name == ppsconsts.SpecRepo {
			specCommit = provCommit
			break
		}
	}
	if specCommit == nil {
		return nil, fmt.Errorf("couldn't find spec commit for job %s, (this is likely a bug)", jobPtr.Job.ID)
	}
	pipelinePtr := &pps.EtcdPipelineInfo{}
	if err := a.pipelines.ReadOnly(pachClient.Ctx()).Get(jobPtr.Pipeline.Name, pipelinePtr); err != nil {
		return nil, err
	}
	pipelineInfo, err := ppsutil.GetPipelineInfo(pachClient, jobPtr.Pipeline.Name, pipelinePtr)
	if err != nil {
		return nil, err
	}
	result.Transform = pipelineInfo.Transform
	result.PipelineVersion = pipelineInfo.Version
	result.ParallelismSpec = pipelineInfo.ParallelismSpec
	result.Egress = pipelineInfo.Egress
	result.Service = pipelineInfo.Service
	result.OutputRepo = &pfs.Repo{Name: jobPtr.Pipeline.Name}
	result.OutputBranch = pipelineInfo.OutputBranch
	result.ResourceRequests = pipelineInfo.ResourceRequests
	result.ResourceLimits = pipelineInfo.ResourceLimits
	result.Input = ppsutil.JobInput(pipelineInfo, commitInfo)
	result.Incremental = pipelineInfo.Incremental
	result.EnableStats = pipelineInfo.EnableStats
	result.Salt = pipelineInfo.Salt
	result.Batch = pipelineInfo.Batch
	result.ChunkSpec = pipelineInfo.ChunkSpec
	result.DatumTimeout = pipelineInfo.DatumTimeout
	result.JobTimeout = pipelineInfo.JobTimeout
	return result, nil
}

func (a *apiServer) ListJob(ctx context.Context, request *pps.ListJobRequest) (response *pps.JobInfos, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) {
		if response != nil && len(response.JobInfo) > client.MaxListItemsLog {
			logrus.Infof("Response contains %d objects; logging the first %d", len(response.JobInfo), client.MaxListItemsLog)
			a.Log(request, &pps.JobInfos{response.JobInfo[:client.MaxListItemsLog]}, retErr, time.Since(start))
		} else {
			a.Log(request, response, retErr, time.Since(start))
		}
	}(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)
	jobInfos, err := a.listJob(pachClient, request.Pipeline, request.OutputCommit, request.InputCommit)
	if err != nil {
		return nil, err
	}
	return &pps.JobInfos{jobInfos}, nil
}

func (a *apiServer) ListJobStream(request *pps.ListJobRequest, resp pps.API_ListJobStreamServer) (retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	sent := 0
	defer func(start time.Time) {
		a.Log(request, fmt.Sprintf("stream containing %d JobInfos", sent), retErr, time.Since(start))
	}(time.Now())
	pachClient := a.getPachClient().WithCtx(resp.Context())
	jobInfos, err := a.listJob(pachClient, request.Pipeline, request.OutputCommit, request.InputCommit)
	if err != nil {
		return err
	}
	for _, ji := range jobInfos {
		if err := resp.Send(ji); err != nil {
			return err
		}
		sent++
	}
	return nil
}

func (a *apiServer) DeleteJob(ctx context.Context, request *pps.DeleteJobRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())

	_, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		return a.jobs.ReadWrite(stm).Delete(request.Job.ID)
	})
	if err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) StopJob(ctx context.Context, request *pps.StopJobRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	_, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		jobs := a.jobs.ReadWrite(stm)
		jobPtr := &pps.EtcdJobInfo{}
		if err := jobs.Get(request.Job.ID, jobPtr); err != nil {
			return err
		}
		return a.updateJobState(stm, jobPtr, pps.JobState_JOB_KILLED)
	})
	if err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) RestartDatum(ctx context.Context, request *pps.RestartDatumRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())

	jobInfo, err := a.InspectJob(ctx, &pps.InspectJobRequest{
		Job: request.Job,
	})
	if err != nil {
		return nil, err
	}
	workerPoolID := ppsutil.PipelineRcName(jobInfo.Pipeline.Name, jobInfo.PipelineVersion)
	if err := cancel(ctx, workerPoolID, a.etcdClient, a.etcdPrefix, request.Job.ID, request.DataFilters); err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

// listDatum contains our internal implementation of ListDatum, which is shared
// between ListDatum and ListDatumStream. When ListDatum is removed, this should
// be inlined into ListDatumStream
func (a *apiServer) listDatum(pachClient *client.APIClient, job *pps.Job, page, pageSize int64) (response *pps.ListDatumResponse, retErr error) {
	response = &pps.ListDatumResponse{}
	ctx := pachClient.Ctx()
	pfsClient := pachClient.PfsAPIClient

	// get information about 'job'
	jobInfo, err := a.InspectJob(ctx, &pps.InspectJobRequest{
		Job: &pps.Job{
			ID: job.ID,
		},
	})
	if err != nil {
		return nil, err
	}

	// authorize ListDatum (must have READER access to all inputs)
	if err := a.authorizePipelineOp(pachClient,
		pipelineOpListDatum,
		jobInfo.Input,
		jobInfo.Pipeline.Name,
	); err != nil {
		return nil, err
	}

	// helper functions for pagination
	getTotalPages := func(totalSize int) int64 {
		return (int64(totalSize) + pageSize - 1) / pageSize // == ceil(totalSize/pageSize)
	}
	getPageBounds := func(totalSize int) (int, int, error) {
		start := int(page * pageSize)
		end := int((page + 1) * pageSize)
		switch {
		case totalSize <= start:
			return 0, 0, io.EOF
		case totalSize <= end:
			return start, totalSize, nil
		case end < totalSize:
			return start, end, nil
		}
		return 0, 0, goerr.New("getPageBounds: unreachable code")
	}

	df, err := workerpkg.NewDatumFactory(pachClient, jobInfo.Input)
	if err != nil {
		return nil, err
	}
	// If there's no stats commit (job not finished), compute datums using jobInfo
	if jobInfo.StatsCommit == nil {
		start := 0
		end := df.Len()
		if pageSize > 0 {
			var err error
			start, end, err = getPageBounds(df.Len())
			if err != nil {
				return nil, err
			}
			response.Page = page
			response.TotalPages = getTotalPages(df.Len())
		}
		var datumInfos []*pps.DatumInfo
		for i := start; i < end; i++ {
			datum := df.Datum(i) // flattened slice of *worker.Input to job
			id := workerpkg.HashDatum(jobInfo.Pipeline.Name, jobInfo.Salt, datum)
			datumInfo := &pps.DatumInfo{
				Datum: &pps.Datum{
					ID:  id,
					Job: jobInfo.Job,
				},
				State: pps.DatumState_STARTING,
			}
			for _, input := range datum {
				datumInfo.Data = append(datumInfo.Data, input.FileInfo)
			}
			datumInfos = append(datumInfos, datumInfo)
		}
		response.DatumInfos = datumInfos
		return response, nil
	}

	// There is a stats commit -- job is finished
	// List the files under / in the stats branch to get all the datums
	file := &pfs.File{
		Commit: jobInfo.StatsCommit,
		Path:   "/",
	}

	var datumFileInfos []*pfs.FileInfo
	fs, err := pfsClient.ListFileStream(ctx, &pfs.ListFileRequest{file, true})
	if err != nil {
		return nil, grpcutil.ScrubGRPC(err)
	}
	// Omit files at the top level that correspond to aggregate job stats
	blacklist := map[string]bool{
		"stats": true,
		"logs":  true,
		"pfs":   true,
	}
	pathToDatumHash := func(path string) (string, error) {
		_, datumHash := filepath.Split(path)
		if _, ok := blacklist[datumHash]; ok {
			return "", fmt.Errorf("value %v is not a datum hash", datumHash)
		}
		return datumHash, nil
	}
	for {
		f, err := fs.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, grpcutil.ScrubGRPC(err)
		}
		if _, err := pathToDatumHash(f.File.Path); err != nil {
			// not a datum
			continue
		}
		datumFileInfos = append(datumFileInfos, f)
	}
	// Sort results (failed first)
	sort.Slice(datumFileInfos, func(i, j int) bool {
		return datumFileToState(datumFileInfos[i], jobInfo.Job.ID) < datumFileToState(datumFileInfos[j], jobInfo.Job.ID)
	})
	if pageSize > 0 {
		response.Page = page
		response.TotalPages = getTotalPages(len(datumFileInfos))
		start, end, err := getPageBounds(len(datumFileInfos))
		if err != nil {
			return nil, err
		}
		datumFileInfos = datumFileInfos[start:end]
	}

	var egGetDatums errgroup.Group
	limiter := limit.New(200)
	datumInfos := make([]*pps.DatumInfo, len(datumFileInfos))
	for index, fileInfo := range datumFileInfos {
		fileInfo := fileInfo
		index := index
		egGetDatums.Go(func() error {
			limiter.Acquire()
			defer limiter.Release()
			datumHash, err := pathToDatumHash(fileInfo.File.Path)
			if err != nil {
				// not a datum
				return nil
			}
			datum, err := a.getDatum(pachClient, jobInfo.StatsCommit.Repo.Name, jobInfo.StatsCommit, job.ID, datumHash, df)
			if err != nil {
				return err
			}
			datumInfos[index] = datum
			return nil
		})
	}
	if err = egGetDatums.Wait(); err != nil {
		return nil, err
	}
	response.DatumInfos = datumInfos
	return response, nil
}

func (a *apiServer) ListDatum(ctx context.Context, request *pps.ListDatumRequest) (response *pps.ListDatumResponse, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) {
		if response != nil && len(response.DatumInfos) > client.MaxListItemsLog {
			logrus.Infof("Response contains %d objects; logging the first %d", len(response.DatumInfos), client.MaxListItemsLog)
			logResponse := &pps.ListDatumResponse{
				TotalPages: response.TotalPages,
				Page:       response.Page,
				DatumInfos: response.DatumInfos[:client.MaxListItemsLog],
			}
			a.Log(request, logResponse, retErr, time.Since(start))
		} else {
			a.Log(request, response, retErr, time.Since(start))
		}
	}(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)
	return a.listDatum(pachClient, request.Job, request.Page, request.PageSize)
}

func (a *apiServer) ListDatumStream(req *pps.ListDatumRequest, resp pps.API_ListDatumStreamServer) (retErr error) {
	func() { a.Log(req, nil, nil, 0) }()
	sent := 0
	defer func(start time.Time) {
		a.Log(req, fmt.Sprintf("stream containing %d DatumInfos", sent), retErr, time.Since(start))
	}(time.Now())
	pachClient := a.getPachClient().WithCtx(resp.Context())
	ldr, err := a.listDatum(pachClient, req.Job, req.Page, req.PageSize)
	if err != nil {
		return err
	}
	first := true
	for _, di := range ldr.DatumInfos {
		r := &pps.ListDatumStreamResponse{}
		if first {
			r.Page = ldr.Page
			r.TotalPages = ldr.TotalPages
			first = false
		}
		r.DatumInfo = di
		if err := resp.Send(r); err != nil {
			return err
		}
		sent++
	}
	return nil
}

func datumFileToState(f *pfs.FileInfo, jobID string) pps.DatumState {
	for _, childFileName := range f.Children {
		if strings.HasPrefix(childFileName, "job") && strings.Split(childFileName, ":")[1] != jobID {
			return pps.DatumState_SKIPPED
		}
		if childFileName == "failure" {
			return pps.DatumState_FAILED
		}
	}
	return pps.DatumState_SUCCESS
}

func (a *apiServer) getDatum(pachClient *client.APIClient, repo string, commit *pfs.Commit, jobID string, datumID string, df workerpkg.DatumFactory) (datumInfo *pps.DatumInfo, retErr error) {
	datumInfo = &pps.DatumInfo{
		Datum: &pps.Datum{
			ID:  datumID,
			Job: &pps.Job{jobID},
		},
		State: pps.DatumState_SUCCESS,
	}
	ctx := pachClient.Ctx()
	pfsClient := pachClient.PfsAPIClient

	// Check if skipped
	fileInfos, err := pachClient.GlobFile(commit.Repo.Name, commit.ID, fmt.Sprintf("/%v/job:*", datumID))
	if err != nil {
		return nil, err
	}
	if len(fileInfos) != 1 {
		return nil, fmt.Errorf("couldn't find job file")
	}
	if strings.Split(fileInfos[0].File.Path, ":")[1] != jobID {
		datumInfo.State = pps.DatumState_SKIPPED
	}

	// Check if failed
	stateFile := &pfs.File{
		Commit: commit,
		Path:   fmt.Sprintf("/%v/failure", datumID),
	}
	_, err = pfsClient.InspectFile(ctx, &pfs.InspectFileRequest{stateFile})
	if err == nil {
		datumInfo.State = pps.DatumState_FAILED
	} else if !isNotFoundErr(err) {
		return nil, err
	}

	// Populate stats
	var buffer bytes.Buffer
	if err := pachClient.GetFile(commit.Repo.Name, commit.ID, fmt.Sprintf("/%v/stats", datumID), 0, 0, &buffer); err != nil {
		return nil, err
	}
	stats := &pps.ProcessStats{}
	err = jsonpb.Unmarshal(&buffer, stats)
	if err != nil {
		return nil, err
	}
	datumInfo.Stats = stats
	buffer.Reset()
	if err := pachClient.GetFile(commit.Repo.Name, commit.ID, fmt.Sprintf("/%v/index", datumID), 0, 0, &buffer); err != nil {
		return nil, err
	}
	i, err := strconv.Atoi(buffer.String())
	if err != nil {
		return nil, err
	}
	if i >= df.Len() {
		return nil, fmt.Errorf("index %d out of range", i)
	}
	inputs := df.Datum(i)
	for _, input := range inputs {
		datumInfo.Data = append(datumInfo.Data, input.FileInfo)
	}
	datumInfo.PfsState = &pfs.File{
		Commit: commit,
		Path:   fmt.Sprintf("/%v/pfs", datumID),
	}

	return datumInfo, nil
}

func (a *apiServer) InspectDatum(ctx context.Context, request *pps.InspectDatumRequest) (response *pps.DatumInfo, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)
	ctx = pachClient.Ctx() // pachClient will propagate auth info
	jobInfo, err := a.InspectJob(ctx, &pps.InspectJobRequest{
		Job: &pps.Job{
			ID: request.Datum.Job.ID,
		},
	})
	if err != nil {
		return nil, err
	}

	if !jobInfo.EnableStats {
		return nil, fmt.Errorf("stats not enabled on %v", jobInfo.Pipeline.Name)
	}
	if jobInfo.StatsCommit == nil {
		return nil, fmt.Errorf("job not finished, no stats output yet")
	}
	df, err := workerpkg.NewDatumFactory(pachClient, jobInfo.Input)
	if err != nil {
		return nil, err
	}

	// Populate datumInfo given a path
	datumInfo, err := a.getDatum(pachClient, jobInfo.StatsCommit.Repo.Name, jobInfo.StatsCommit, request.Datum.Job.ID, request.Datum.ID, df)
	if err != nil {
		return nil, err
	}

	return datumInfo, nil
}

func (a *apiServer) GetLogs(request *pps.GetLogsRequest, apiGetLogsServer pps.API_GetLogsServer) (retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, nil, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(apiGetLogsServer.Context())
	ctx := pachClient.Ctx() // pachClient will propagate auth info

	// Authorize request and get list of pods containing logs we're interested in
	// (based on pipeline and job filters)
	var rcName, containerName string
	if request.Pipeline == nil && request.Job == nil {
		// no authorization is done to get logs from master
		containerName, rcName = "pachd", "pachd"
	} else {
		containerName = client.PPSWorkerUserContainerName

		// 1) Lookup the PipelineInfo for this pipeline/job, for auth and to get the
		// RC name
		var pipelineInfo *pps.PipelineInfo
		var statsCommit *pfs.Commit
		var err error
		if request.Pipeline != nil {
			pipelineInfo, err = a.inspectPipeline(pachClient, request.Pipeline.Name)
		} else if request.Job != nil {
			// If user provides a job, lookup the pipeline from the job info, and then
			// get the pipeline RC
			var jobPtr pps.EtcdJobInfo
			err = a.jobs.ReadOnly(ctx).Get(request.Job.ID, &jobPtr)
			if err != nil {
				return fmt.Errorf("could not get job information for \"%s\": %v", request.Job.ID, err)
			}
			statsCommit = jobPtr.StatsCommit
			pipelineInfo, err = a.inspectPipeline(pachClient, jobPtr.Pipeline.Name)
		}
		if err != nil {
			return fmt.Errorf("could not get pipeline information for %s: %v", request.Pipeline.Name, err)
		}

		// 2) Check whether the caller is authorized to get logs from this pipeline/job
		if err := a.authorizePipelineOp(pachClient, pipelineOpGetLogs, pipelineInfo.Input, pipelineInfo.Pipeline.Name); err != nil {
			return err
		}

		// If the job had stats enabled, we use the logs from the stats
		// commit since that's likely to yield better results.
		if statsCommit != nil {
			return a.getLogsFromStats(pachClient, request, apiGetLogsServer, statsCommit)
		}

		// 3) Get rcName for this pipeline
		rcName = ppsutil.PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version)
		if err != nil {
			return err
		}
	}

	// Get pods managed by the RC we're scraping (either pipeline or pachd)
	pods, err := a.rcPods(rcName)
	if err != nil {
		return fmt.Errorf("could not get pods in rc \"%s\" containing logs: %s", rcName, err.Error())
	}
	if len(pods) == 0 {
		return fmt.Errorf("no pods belonging to the rc \"%s\" were found", rcName)
	}

	// Spawn one goroutine per pod. Each goro writes its pod's logs to a channel
	// and channels are read into the output server in a stable order.
	// (sort the pods to make sure that the order of log lines is stable)
	sort.Sort(podSlice(pods))
	logCh := make(chan *pps.LogMessage)
	var eg errgroup.Group
	var mu sync.Mutex
	eg.Go(func() error {
		for _, pod := range pods {
			pod := pod
			if !request.Follow {
				mu.Lock()
			}
			eg.Go(func() (retErr error) {
				if !request.Follow {
					defer mu.Unlock()
				}
				// Get full set of logs from pod i
				stream, err := a.kubeClient.CoreV1().Pods(a.namespace).GetLogs(
					pod.ObjectMeta.Name, &v1.PodLogOptions{
						Container: containerName,
						Follow:    request.Follow,
						TailLines: &request.Tail,
					}).Timeout(10 * time.Second).Stream()
				if err != nil {
					if apiStatus, ok := err.(errors.APIStatus); ok &&
						strings.Contains(apiStatus.Status().Message, "PodInitializing") {
						return nil // No logs to collect from this node yet, just skip it
					}
					return err
				}
				defer func() {
					if err := stream.Close(); err != nil && retErr == nil {
						retErr = err
					}
				}()

				// Parse pods' log lines, and filter out irrelevant ones
				scanner := bufio.NewScanner(stream)
				for scanner.Scan() {
					msg := new(pps.LogMessage)
					if containerName == "pachd" {
						msg.Message = scanner.Text() + "\n"
					} else {
						logBytes := scanner.Bytes()
						if err := jsonpb.Unmarshal(bytes.NewReader(logBytes), msg); err != nil {
							continue
						}

						// Filter out log lines that don't match on pipeline or job
						if request.Pipeline != nil && request.Pipeline.Name != msg.PipelineName {
							continue
						}
						if request.Job != nil && request.Job.ID != msg.JobID {
							continue
						}
						if request.Datum != nil && request.Datum.ID != msg.DatumID {
							continue
						}
						if request.Master != msg.Master {
							continue
						}
						if !workerpkg.MatchDatum(request.DataFilters, msg.Data) {
							continue
						}
					}

					// Log message passes all filters -- return it
					select {
					case logCh <- msg:
					case <-ctx.Done():
						return nil
					}
				}
				return nil
			})
		}
		return nil
	})
	var egErr error
	go func() {
		egErr = eg.Wait()
		close(logCh)
	}()

	for msg := range logCh {
		if err := apiGetLogsServer.Send(msg); err != nil {
			return err
		}
	}
	return egErr
}

func (a *apiServer) getLogsFromStats(pachClient *client.APIClient, request *pps.GetLogsRequest, apiGetLogsServer pps.API_GetLogsServer, statsCommit *pfs.Commit) error {
	pfsClient := pachClient.PfsAPIClient
	fs, err := pfsClient.GlobFileStream(pachClient.Ctx(), &pfs.GlobFileRequest{
		Commit:  statsCommit,
		Pattern: "*/logs", // this is the path where logs reside
	})
	if err != nil {
		return grpcutil.ScrubGRPC(err)
	}

	limiter := limit.New(20)
	var eg errgroup.Group
	var mu sync.Mutex
	for {
		fileInfo, err := fs.Recv()
		if err == io.EOF {
			break
		}
		eg.Go(func() error {
			if err != nil {
				return err
			}
			limiter.Acquire()
			defer limiter.Release()
			var buf bytes.Buffer
			if err := pachClient.GetFile(fileInfo.File.Commit.Repo.Name, fileInfo.File.Commit.ID, fileInfo.File.Path, 0, 0, &buf); err != nil {
				return err
			}
			// Parse pods' log lines, and filter out irrelevant ones
			scanner := bufio.NewScanner(&buf)
			for scanner.Scan() {
				logBytes := scanner.Bytes()
				msg := new(pps.LogMessage)
				if err := jsonpb.Unmarshal(bytes.NewReader(logBytes), msg); err != nil {
					continue
				}
				if request.Pipeline != nil && request.Pipeline.Name != msg.PipelineName {
					continue
				}
				if request.Job != nil && request.Job.ID != msg.JobID {
					continue
				}
				if request.Datum != nil && request.Datum.ID != msg.DatumID {
					continue
				}
				if request.Master != msg.Master {
					continue
				}
				if !workerpkg.MatchDatum(request.DataFilters, msg.Data) {
					continue
				}

				mu.Lock()
				if err := apiGetLogsServer.Send(msg); err != nil {
					mu.Unlock()
					return err
				}
				mu.Unlock()
			}
			return nil
		})
	}
	return eg.Wait()
}

func (a *apiServer) validatePipeline(pachClient *client.APIClient, pipelineInfo *pps.PipelineInfo) error {
	if err := a.validateInput(pachClient, pipelineInfo.Pipeline.Name, pipelineInfo.Input, false); err != nil {
		return err
	}
	if err := validateTransform(pipelineInfo.Transform); err != nil {
		return fmt.Errorf("invalid transform: %v", err)
	}
	if pipelineInfo.ParallelismSpec != nil {
		if pipelineInfo.ParallelismSpec.Constant < 0 {
			return fmt.Errorf("ParallelismSpec.Constant must be > 0")
		}
		if pipelineInfo.ParallelismSpec.Coefficient < 0 {
			return fmt.Errorf("ParallelismSpec.Coefficient must be > 0")
		}
		if pipelineInfo.ParallelismSpec.Constant != 0 &&
			pipelineInfo.ParallelismSpec.Coefficient != 0 {
			return fmt.Errorf("contradictory parallelism strategies: must set at " +
				"most one of ParallelismSpec.Constant and ParallelismSpec.Coefficient")
		}
		if pipelineInfo.Service != nil && pipelineInfo.ParallelismSpec.Constant != 1 {
			return fmt.Errorf("services can only be run with a constant parallelism of 1")
		}
	}
	if pipelineInfo.OutputBranch == "" {
		return fmt.Errorf("pipeline needs to specify an output branch")
	}
	if _, err := resource.ParseQuantity(pipelineInfo.CacheSize); err != nil {
		return fmt.Errorf("could not parse cacheSize '%s': %v", pipelineInfo.CacheSize, err)
	}
	if pipelineInfo.Incremental {
		// for incremental jobs we can't have shared provenance
		key := path.Join
		provMap := make(map[string]bool)
		for _, branch := range pps.InputBranches(pipelineInfo.Input) {
			// Add the branches themselves to provMap
			if provMap[key(branch.Repo.Name, branch.Name)] {
				return fmt.Errorf("can't create an incremental pipeline with inputs that share provenance")
			}
			provMap[key(branch.Repo.Name, branch.Name)] = true
			// Add the input branches' provenance to provMap
			resp, err := pachClient.InspectBranch(branch.Repo.Name, branch.Name)
			if err != nil {
				if isNotFoundErr(err) {
					continue // input branch doesn't exist--will be created w/ empty provenance
				}
				return err
			}
			for _, provBranch := range resp.Provenance {
				if provMap[key(provBranch.Repo.Name, provBranch.Name)] {
					return fmt.Errorf("can't create an incremental pipeline with inputs that share provenance")
				}
				provMap[key(provBranch.Repo.Name, provBranch.Name)] = true
			}
		}
	}
	if pipelineInfo.JobTimeout != nil {
		_, err := types.DurationFromProto(pipelineInfo.JobTimeout)
		if err != nil {
			return err
		}
	}
	if pipelineInfo.DatumTimeout != nil {
		_, err := types.DurationFromProto(pipelineInfo.DatumTimeout)
		if err != nil {
			return err
		}
	}
	return nil
}

// authorizing a pipeline operation varies slightly depending on whether the
// pipeline is being created, updated, or deleted
type pipelineOperation uint8

const (
	// pipelineOpCreate is required for CreatePipeline
	pipelineOpCreate pipelineOperation = iota
	// pipelineOpListDatum is required for ListDatum
	pipelineOpListDatum
	// pipelineOpGetLogs is required for GetLogs
	pipelineOpGetLogs
	// pipelineOpUpdate is required for UpdatePipeline
	pipelineOpUpdate
	// pipelineOpUpdate is required for DeletePipeline
	pipelineOpDelete
)

// authorizePipelineOp checks if the user indicated by 'ctx' is authorized
// to perform 'operation' on the pipeline in 'info'
func (a *apiServer) authorizePipelineOp(pachClient *client.APIClient, operation pipelineOperation, input *pps.Input, output string) error {
	ctx := pachClient.Ctx()
	if _, err := pachClient.WhoAmI(ctx, &auth.WhoAmIRequest{}); err != nil {
		if auth.IsNotActivatedError(err) {
			return nil // Auth isn't activated, user may proceed
		}
		return err
	}

	// Check that the user is authorized to read all input repos, and write to the
	// output repo (which the pipeline needs to be able to do on the user's
	// behalf)
	var eg errgroup.Group
	done := make(map[string]struct{}) // don't double-authorize repos
	pps.VisitInput(input, func(in *pps.Input) {
		if in.Atom == nil {
			return
		}
		repo := in.Atom.Repo
		if _, ok := done[repo]; ok {
			return
		}
		done[in.Atom.Repo] = struct{}{}
		eg.Go(func() error {
			resp, err := pachClient.Authorize(ctx, &auth.AuthorizeRequest{
				Repo:  repo,
				Scope: auth.Scope_READER,
			})
			if err != nil {
				return err
			}
			if !resp.Authorized {
				return &auth.NotAuthorizedError{
					Repo:     repo,
					Required: auth.Scope_READER,
				}
			}
			return nil
		})
	})
	if err := eg.Wait(); err != nil {
		return err
	}

	// Check that the user is authorized to write to the output repo.
	// Note: authorizePipelineOp is called before CreateRepo creates a
	// PipelineInfo proto in etcd, so PipelineManager won't have created an output
	// repo yet, and it's possible to check that the output repo doesn't exist
	// (if it did exist, we'd have to check that the user has permission to write
	// to it, and this is simpler)
	var required auth.Scope
	switch operation {
	case pipelineOpCreate:
		if _, err := pachClient.InspectRepo(output); err == nil {
			return fmt.Errorf("cannot overwrite repo \"%s\" with new output repo", output)
		} else if !isNotFoundErr(err) {
			return err
		}
	case pipelineOpListDatum, pipelineOpGetLogs:
		required = auth.Scope_READER
	case pipelineOpUpdate:
		required = auth.Scope_WRITER
	case pipelineOpDelete:
		required = auth.Scope_OWNER
	default:
		return fmt.Errorf("internal error, unrecognized operation %v", operation)
	}
	if required != auth.Scope_NONE {
		resp, err := pachClient.Authorize(ctx, &auth.AuthorizeRequest{
			Repo:  output,
			Scope: required,
		})
		if err != nil {
			return err
		}
		if !resp.Authorized {
			return &auth.NotAuthorizedError{
				Repo:     output,
				Required: required,
			}
		}
	}
	return nil
}

func branchProvenance(input *pps.Input) []*pfs.Branch {
	var result []*pfs.Branch
	pps.VisitInput(input, func(input *pps.Input) {
		if input.Atom != nil {
			result = append(result, client.NewBranch(input.Atom.Repo, input.Atom.Branch))
		}
		if input.Cron != nil {
			result = append(result, client.NewBranch(input.Cron.Repo, "master"))
		}
		if input.Git != nil {
			result = append(result, client.NewBranch(input.Git.Name, input.Git.Branch))
		}
	})
	return result
}

// hardStopPipeline does essentially the same thing as StopPipeline (deletes the
// pipeline's branch provenance, deletes any open commits, deletes any k8s
// workers), but does it immediately. This is to avoid races between operations
// that will do subsequent work (e.g. UpdatePipeline and DeletePipeline) and the
// PPS master
func (a *apiServer) hardStopPipeline(pachClient *client.APIClient, pipelineInfo *pps.PipelineInfo) error {
	// Remove the output branch's provenance so that no new jobs can be created
	if err := pachClient.CreateBranch(
		pipelineInfo.Pipeline.Name,
		pipelineInfo.OutputBranch,
		pipelineInfo.OutputBranch,
		nil,
	); err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("could not rename original output branch: %v", err)
	}

	// Now that new commits won't be created on the master branch, enumerate
	// existing commits and close any open ones.
	iter, err := pachClient.ListCommitStream(pachClient.Ctx(), &pfs.ListCommitRequest{
		Repo: client.NewRepo(pipelineInfo.Pipeline.Name),
		To:   client.NewCommit(pipelineInfo.Pipeline.Name, pipelineInfo.OutputBranch),
	})
	if err != nil {
		return fmt.Errorf("couldn't get open commits on '%s': %v", pipelineInfo.OutputBranch, err)
	}
	// Finish all open commits, most recent first (so that we finish the
	// current job's output commit--the oldest--last, and unblock the master
	// only after all other commits are also finished, preventing any new jobs)
	for {
		ci, err := iter.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if ci.Finished == nil {
			// Finish the commit and don't pass a tree
			pachClient.PfsAPIClient.FinishCommit(pachClient.Ctx(), &pfs.FinishCommitRequest{
				Commit: ci.Commit,
				Empty:  true,
			})
		}
	}
	return nil
}

// ppsToken is the cached auth token used by PPS to write to the spec repo.
// ppsTokenOnce ensures that ppsToken is only read from etcd once. These are
// read/written by getPPSToken()
var (
	ppsToken     string
	ppsTokenOnce sync.Once
)

// getPPSToken returns the auth token used by PPS to write to the spec repo.
// Using this token grants any PPS request admin-level authority, so its use is
// restricted to makePipelineInfoCommit(), deletePipelineInfo(), and master()
func (a *apiServer) getPPSToken() string {
	// Get PPS auth token
	ppsTokenOnce.Do(func() {
		resp, err := a.etcdClient.Get(context.Background(),
			path.Join(a.etcdPrefix, ppsconsts.PPSTokenKey))
		if err != nil {
			panic(fmt.Sprintf("could not read PPS token: %v", err))
		}
		if resp.Count != 1 {
			panic(fmt.Sprintf("got an unexpected number of PPS tokens: %d", resp.Count))
		}
		ppsToken = string(resp.Kvs[0].Value)
	})
	return ppsToken
}

// makePipelineInfoComit is a helper for CreatePipeline that creates a commit
// with 'pipelineInfo' in SpecRepo (in PFS). It's called in both the case where
// a user is updating a pipeline and the case where a user is creating a new
// pipeline
func (a *apiServer) makePipelineInfoComit(pachClient *client.APIClient, pipelineInfo *pps.PipelineInfo, update bool) (*pfs.Commit, error) {
	// copy pachClient, so we can overwrite the auth token
	pachClientCopy := *pachClient
	pachClient = &pachClientCopy
	// Get Pipeline name
	pipelineName := pipelineInfo.Pipeline.Name

	// Set the pach client's auth token to the master token. At this point, no
	// parameters to any pachClient requests should be unvalidated user input, as
	// the pachClient has admin-level authority
	pachClient.SetAuthToken(a.getPPSToken())

	// If we're creating a new pipeline, create the pipeline branch
	if !update {
		// Create pipeline branch in spec repo and write PipelineInfo there
		if _, err := pachClient.InspectBranch(ppsconsts.SpecRepo, pipelineName); err == nil {
			return nil, fmt.Errorf("pipeline spec branch for \"%s\" already exists: delete it with DeletePipeline", pipelineName, ppsconsts.SpecRepo)
		}
		if err := pachClient.CreateBranch(ppsconsts.SpecRepo, pipelineName, "", nil); err != nil {
			return nil, fmt.Errorf("could not create pipeline spec branch for \"%s\" in %s: %v", pipelineName, ppsconsts.SpecRepo, err)
		}
	}

	commit, err := pachClient.StartCommit(ppsconsts.SpecRepo, pipelineName)
	if err != nil {
		return nil, err
	}
	// Delete the old PipelineInfo (if it exists), otherwise the new
	// PipelineInfo's bytes will be appended to the old bytes
	if err := pachClient.DeleteFile(
		ppsconsts.SpecRepo, commit.ID, ppsconsts.SpecFile,
	); err != nil && !strings.Contains(err.Error(), "not found") {
		return nil, err
	}

	data, err := pipelineInfo.Marshal()
	if err != nil {
		return nil, fmt.Errorf("could not marshal PipelineInfo: %v", err)
	}
	if _, err := pachClient.PutFile(ppsconsts.SpecRepo, commit.ID, ppsconsts.SpecFile, bytes.NewReader(data)); err != nil {
		return nil, err
	}
	if err := pachClient.FinishCommit(ppsconsts.SpecRepo, commit.ID); err != nil {
		return nil, err
	}
	return commit, nil
}

func (a *apiServer) CreatePipeline(ctx context.Context, request *pps.CreatePipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "CreatePipeline")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)
	ctx = pachClient.Ctx() // pachClient will propagate auth info
	pfsClient := pachClient.PfsAPIClient

	pipelineInfo := &pps.PipelineInfo{
		Pipeline:           request.Pipeline,
		Version:            1,
		Transform:          request.Transform,
		ParallelismSpec:    request.ParallelismSpec,
		Input:              request.Input,
		OutputBranch:       request.OutputBranch,
		Egress:             request.Egress,
		CreatedAt:          now(),
		ScaleDownThreshold: request.ScaleDownThreshold,
		ResourceRequests:   request.ResourceRequests,
		ResourceLimits:     request.ResourceLimits,
		Description:        request.Description,
		Incremental:        request.Incremental,
		CacheSize:          request.CacheSize,
		EnableStats:        request.EnableStats,
		Salt:               uuid.NewWithoutDashes(),
		Batch:              request.Batch,
		MaxQueueSize:       request.MaxQueueSize,
		Service:            request.Service,
		ChunkSpec:          request.ChunkSpec,
		DatumTimeout:       request.DatumTimeout,
		JobTimeout:         request.JobTimeout,
	}
	setPipelineDefaults(pipelineInfo)

	// Validate new pipeline
	if err := a.validatePipeline(pachClient, pipelineInfo); err != nil {
		return nil, err
	}
	var visitErr error
	pps.VisitInput(pipelineInfo.Input, func(input *pps.Input) {
		if input.Cron != nil {
			if err := pachClient.CreateRepo(input.Cron.Repo); err != nil && !isAlreadyExistsErr(err) {
				visitErr = err
			}
		}
		if input.Git != nil {
			if err := pachClient.CreateRepo(input.Git.Name); err != nil && !isAlreadyExistsErr(err) {
				visitErr = err
			}
		}
	})
	if visitErr != nil {
		return nil, visitErr
	}

	// Authorize pipeline creation
	operation := pipelineOpCreate
	if request.Update {
		operation = pipelineOpUpdate
	}
	if err := a.authorizePipelineOp(pachClient, operation, pipelineInfo.Input, pipelineInfo.Pipeline.Name); err != nil {
		return nil, err
	}
	// User is authorized -- get capability token (copy to pipeline in STM below)
	capabilityResp, err := pachClient.GetCapability(ctx, &auth.GetCapabilityRequest{})
	if err != nil {
		return nil, fmt.Errorf("error getting capability for the user: %v", err)
	}

	pipelineName := pipelineInfo.Pipeline.Name
	pps.SortInput(pipelineInfo.Input) // Makes datum hashes comparable
	if request.Update {
		// Help user fix inconsistency if previous UpdatePipeline call failed
		if ci, err := pachClient.InspectCommit(ppsconsts.SpecRepo, pipelineName); err != nil {
			return nil, err
		} else if ci.Finished == nil {
			return nil, fmt.Errorf("the HEAD commit of this pipeline's spec branch " +
				"is open. Either another CreatePipeline call is running or a previous " +
				"call crashed. If you're sure no other CreatePipeline commands are " +
				"running, you can run 'pachctl update-pipeline --clean' which will " +
				"delete this open commit")
		}

		a.hardStopPipeline(pachClient, pipelineInfo)

		// Look up pipelineInfo and update it, writing updated pipelineInfo back to
		// PFS in a new commit. Do this inside an etcd transaction as PFS doesn't
		// support transactions and this prevents concurrent UpdatePipeline calls
		// from racing
		var oldCapability string
		if _, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
			pipelines := a.pipelines.ReadWrite(stm)
			// Read existing PipelineInfo from PFS output repo
			var err error
			pipelinePtr := pps.EtcdPipelineInfo{}
			if err := pipelines.Get(pipelineName, &pipelinePtr); err != nil {
				return err
			}
			oldCapability = pipelinePtr.Capability
			oldPipelineInfo, err := ppsutil.GetPipelineInfo(pachClient, pipelineName, &pipelinePtr)
			if err != nil {
				return err
			}

			// Modify pipelineInfo
			pipelineInfo.Version = oldPipelineInfo.Version + 1
			if !request.Reprocess {
				pipelineInfo.Salt = oldPipelineInfo.Salt
			}

			// Write updated PipelineInfo back to PFS.
			commit, err := a.makePipelineInfoComit(pachClient, pipelineInfo, request.Update)
			if err != nil {
				return err
			}
			// Write updated pointer back to etcd
			pipelinePtr.SpecCommit = commit
			pipelinePtr.Capability = capabilityResp.Capability
			return pipelines.Put(pipelineName, &pipelinePtr)
		}); err != nil {
			return nil, err
		}

		// Update has succeeded, Revoke the old capability retrieved from pipelineInfo
		if oldCapability != "" {
			if _, err := pachClient.RevokeAuthToken(ctx, &auth.RevokeAuthTokenRequest{
				Token: oldCapability,
			}); err != nil && !auth.IsNotActivatedError(err) {
				return nil, fmt.Errorf("error revoking old capability: %v", err)
			}
		}
	} else {
		// Create output repo, where we'll store the pipeline spec, future pipeline
		// output, and pipeline stats
		if _, err := pfsClient.CreateRepo(ctx, &pfs.CreateRepoRequest{
			Repo: &pfs.Repo{pipelineName},
		}); err != nil && !isAlreadyExistsErr(err) {
			return nil, err
		}
		commit, err := a.makePipelineInfoComit(pachClient, pipelineInfo, request.Update)
		if err != nil {
			return nil, err
		}
		// Put a pointer to the new PipelineInfo commit into etcd
		_, err = col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
			err = a.pipelines.ReadWrite(stm).Create(pipelineName, &pps.EtcdPipelineInfo{
				SpecCommit: commit,
				State:      pps.PipelineState_PIPELINE_STARTING,
				Capability: capabilityResp.Capability,
			})
			if isAlreadyExistsErr(err) {
				pachClient.DeleteCommit(pipelineName, commit.ID)
				return newErrPipelineExists(pipelineName)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}

	// Create a branch for the pipeline's output data (provenant on the spec branch)
	provenance := append(branchProvenance(pipelineInfo.Input),
		client.NewBranch(ppsconsts.SpecRepo, pipelineName))
	if _, err := pfsClient.CreateBranch(ctx, &pfs.CreateBranchRequest{
		Branch:     client.NewBranch(pipelineName, pipelineInfo.OutputBranch),
		Provenance: provenance,
	}); err != nil {
		return nil, fmt.Errorf("could not update output branch provenance: %v", err)
	}

	return &types.Empty{}, nil
}

// setPipelineDefaults sets the default values for a pipeline info
func setPipelineDefaults(pipelineInfo *pps.PipelineInfo) {
	now := time.Now()
	if pipelineInfo.Transform.Image == "" {
		pipelineInfo.Transform.Image = DefaultUserImage
	}
	pps.VisitInput(pipelineInfo.Input, func(input *pps.Input) {
		if input.Atom != nil {
			if input.Atom.Branch == "" {
				input.Atom.Branch = "master"
			}
			if input.Atom.Name == "" {
				input.Atom.Name = input.Atom.Repo
			}
		}
		if input.Cron != nil {
			if input.Cron.Start == nil {
				start, _ := types.TimestampProto(now)
				input.Cron.Start = start
			}
			if input.Cron.Repo == "" {
				input.Cron.Repo = fmt.Sprintf("%s_%s", pipelineInfo.Pipeline.Name, input.Cron.Name)
			}
		}
		if input.Git != nil {
			if input.Git.Branch == "" {
				input.Git.Branch = "master"
			}
			if input.Git.Name == "" {
				// We know URL looks like:
				// "https://github.com/sjezewski/testgithook.git",
				tokens := strings.Split(path.Base(input.Git.URL), ".")
				input.Git.Name = tokens[0]
			}
		}
	})
	if pipelineInfo.OutputBranch == "" {
		// Output branches default to master
		pipelineInfo.OutputBranch = "master"
	}
	if pipelineInfo.CacheSize == "" {
		pipelineInfo.CacheSize = "64M"
	}
	if pipelineInfo.ResourceRequests == nil && pipelineInfo.CacheSize != "" {
		pipelineInfo.ResourceRequests = &pps.ResourceSpec{
			Memory: pipelineInfo.CacheSize,
		}
	}
	if pipelineInfo.MaxQueueSize < 1 {
		pipelineInfo.MaxQueueSize = 1
	}
}

func (a *apiServer) InspectPipeline(ctx context.Context, request *pps.InspectPipelineRequest) (response *pps.PipelineInfo, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)
	return a.inspectPipeline(pachClient, request.Pipeline.Name)
}

// inspectPipeline contains the functional implementation of InspectPipeline.
// Many functions (GetLogs, ListPipeline, CreateJob) need to inspect a pipeline,
// so they call this instead of making an RPC
func (a *apiServer) inspectPipeline(pachClient *client.APIClient, name string) (*pps.PipelineInfo, error) {
	pipelinePtr := pps.EtcdPipelineInfo{}
	if err := a.pipelines.ReadOnly(pachClient.Ctx()).Get(name, &pipelinePtr); err != nil {
		if col.IsErrNotFound(err) {
			return nil, fmt.Errorf("pipeline \"%s\" not found", name)
		}
		return nil, err
	}
	pipelineInfo, err := ppsutil.GetPipelineInfo(pachClient, name, &pipelinePtr)
	if err != nil {
		return nil, err
	}
	var hasGitInput bool
	pps.VisitInput(pipelineInfo.Input, func(input *pps.Input) {
		if input.Git != nil {
			hasGitInput = true
		}
	})
	if hasGitInput {
		pipelineInfo.GithookURL = "pending"
		svc, err := getGithookService(a.kubeClient, a.namespace)
		if err != nil {
			return pipelineInfo, nil
		}
		numIPs := len(svc.Status.LoadBalancer.Ingress)
		if numIPs == 0 {
			// When running locally, no external IP is set
			return pipelineInfo, nil
		}
		if numIPs != 1 {
			return nil, fmt.Errorf("unexpected number of external IPs set for githook service")
		}
		ingress := svc.Status.LoadBalancer.Ingress[0]
		if ingress.IP != "" {
			// GKE load balancing
			pipelineInfo.GithookURL = githook.URLFromDomain(ingress.IP)
		} else if ingress.Hostname != "" {
			// AWS load balancing
			pipelineInfo.GithookURL = githook.URLFromDomain(ingress.Hostname)
		}
	}
	return pipelineInfo, nil
}

func (a *apiServer) ListPipeline(ctx context.Context, request *pps.ListPipelineRequest) (response *pps.PipelineInfos, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) {
		if response != nil && len(response.PipelineInfo) > client.MaxListItemsLog {
			logrus.Infof("Response contains %d objects; logging the first %d", len(response.PipelineInfo), client.MaxListItemsLog)
			a.Log(request, &pps.PipelineInfos{response.PipelineInfo[:client.MaxListItemsLog]}, retErr, time.Since(start))
		} else {
			a.Log(request, response, retErr, time.Since(start))
		}
	}(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)

	pipelineIter, err := a.pipelines.ReadOnly(pachClient.Ctx()).List()
	if err != nil {
		return nil, err
	}

	pipelineInfos := new(pps.PipelineInfos)
	for {
		var pipelineName string
		pipelinePtr := pps.EtcdPipelineInfo{}
		ok, err := pipelineIter.Next(&pipelineName, &pipelinePtr)
		pipelineName = path.Base(pipelineName) // pipelineIter returns etcd keys
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		// Read existing PipelineInfo from PFS output repo
		// TODO this won't work with auth, as a user now can't call InspectPipeline
		// unless they have READER access to the pipeline's output repo
		pipelineInfo, err := ppsutil.GetPipelineInfo(pachClient, pipelineName, &pipelinePtr)
		if err != nil {
			return nil, err
		}
		pipelineInfos.PipelineInfo = append(pipelineInfos.PipelineInfo, pipelineInfo)
	}
	return pipelineInfos, nil
}

func (a *apiServer) DeletePipeline(ctx context.Context, request *pps.DeletePipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)

	// Possibly list pipelines in etcd (skip PFS read--don't need it) and delete them
	if request.All {
		request.Pipeline = &pps.Pipeline{}
		pipelineIter, err := a.pipelines.ReadOnly(ctx).List()
		if err != nil {
			return nil, err
		}

		for {
			var pipelineName string
			pipelinePtr := pps.EtcdPipelineInfo{}
			ok, err := pipelineIter.Next(&pipelineName, &pipelinePtr)
			pipelineName = path.Base(pipelineName) // pipelineIter returns etcd keys
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
			request.Pipeline.Name = pipelineName
			if _, err := a.deletePipeline(pachClient, request); err != nil {
				return nil, err
			}
		}
		return &types.Empty{}, nil
	}

	// Otherwise delete single pipeline from request
	return a.deletePipeline(pachClient, request)
}

// deletePipelineBranch is a helper for DeletePipeline that deletes a pipeline
// branch in SpecRepo (in PFS)
func (a *apiServer) deletePipelineBranch(pachClient *client.APIClient, pipeline string) error {
	// copy pachClient, so we can overwrite the auth token
	pachClientCopy := *pachClient
	pachClient = &pachClientCopy

	// Set the pach client's auth token to the master token. At this point, no
	// parameters to any pachClient requests should be unvalidated user input, as
	// the pachClient has admin-level authority
	pachClient.SetAuthToken(a.getPPSToken())
	return pachClient.DeleteBranch(ppsconsts.SpecRepo, pipeline)
}

func (a *apiServer) deletePipeline(pachClient *client.APIClient, request *pps.DeletePipelineRequest) (response *types.Empty, retErr error) {
	ctx := pachClient.Ctx() // pachClient will propagate auth info

	// Check if there's an EtcdPipelineInfo for this pipeline. If not, we can't
	// authorize, and must return something here
	pipelinePtr := pps.EtcdPipelineInfo{}
	if err := a.pipelines.ReadOnly(ctx).Get(request.Pipeline.Name, &pipelinePtr); err != nil {
		if col.IsErrNotFound(err) {
			// Check if there's an pipeline branch in the Spec repo.
			// If the spec branch is empty, PFS is in an invalid state: just delete
			// the spec branch and return
			specBranchInfo, err := pachClient.InspectBranch(ppsconsts.SpecRepo, request.Pipeline.Name)
			if err == nil && specBranchInfo.Head == nil {
				if err := a.deletePipelineBranch(pachClient, request.Pipeline.Name); err != nil {
					return nil, err
				}
				return &types.Empty{}, nil
			}
			// No spec branch and no etcd pointer == the pipeline doesn't exist
			return nil, fmt.Errorf("pipeline %v was not found: %v", request.Pipeline.Name, err)
		}
		return nil, err
	}

	// Get current pipeline info from EtcdPipelineInfo (which may not be the spec
	// branch HEAD)
	pipelineInfo, err := a.inspectPipeline(pachClient, request.Pipeline.Name)
	if err != nil {
		return nil, err
	}

	// Check if the caller is authorized to delete this pipeline. This must be
	// done after cleaning up the spec branch HEAD commit, because the
	// authorization condition depends on the pipeline's PipelineInfo
	if err := a.authorizePipelineOp(pachClient, pipelineOpDelete, pipelineInfo.Input, pipelineInfo.Pipeline.Name); err != nil {
		return nil, err
	}

	// Stop this pipeline (inline, so we don't break the PPS master by deleting
	// the pipeline's PipelineInfo in PFS, which we do below)
	a.hardStopPipeline(pachClient, pipelineInfo)

	// Delete pipeline's workers
	if err := a.deleteWorkersForPipeline(pipelineInfo); err != nil {
		return nil, err
	}

	// Revoke the pipeline's capability
	if pipelinePtr.Capability != "" {
		if _, err := pachClient.RevokeAuthToken(ctx, &auth.RevokeAuthTokenRequest{
			Token: pipelinePtr.Capability,
		}); err != nil && !auth.IsNotActivatedError(err) {
			return nil, fmt.Errorf("error revoking old capability: %v", err)
		}
	}

	// Kill or delete all of the pipeline's jobs
	iter, err := a.jobs.ReadOnly(ctx).GetByIndex(ppsdb.JobsPipelineIndex, request.Pipeline)
	if err != nil {
		return nil, err
	}
	for {
		var jobID string
		var jobPtr pps.EtcdJobInfo
		ok, err := iter.Next(&jobID, &jobPtr)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if _, err := a.DeleteJob(ctx, &pps.DeleteJobRequest{&pps.Job{jobID}}); err != nil {
			return nil, err
		}
	}

	var eg errgroup.Group
	// Delete pipeline branch in SpecRepo (leave commits, to preserve downstream
	// commits)
	eg.Go(func() error {
		return a.deletePipelineBranch(pachClient, request.Pipeline.Name)
	})
	// Delete EtcdPipelineInfo
	eg.Go(func() error {
		if _, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
			return a.pipelines.ReadWrite(stm).Delete(request.Pipeline.Name)
		}); err != nil {
			return fmt.Errorf("collection.Delete: %v", err)
		}
		return nil
	})
	// Delete output repo
	eg.Go(func() error {
		return pachClient.DeleteRepo(request.Pipeline.Name, true)
	})
	// Delete cron input repos
	pps.VisitInput(pipelineInfo.Input, func(input *pps.Input) {
		if input.Cron != nil {
			eg.Go(func() error {
				return pachClient.DeleteRepo(input.Cron.Repo, true)
			})
		}
	})
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) StartPipeline(ctx context.Context, request *pps.StartPipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)

	// Get request.Pipeline's info
	pipelineInfo, err := a.inspectPipeline(pachClient, request.Pipeline.Name)
	if err != nil {
		return nil, err
	}

	// check if the caller is authorized to update this pipeline
	if err := a.authorizePipelineOp(pachClient, pipelineOpUpdate, pipelineInfo.Input, pipelineInfo.Pipeline.Name); err != nil {
		return nil, err
	}

	// Replace missing branch provenance (removed by StopPipeline)
	provenance := append(branchProvenance(pipelineInfo.Input),
		client.NewBranch(ppsconsts.SpecRepo, pipelineInfo.Pipeline.Name))
	if err := pachClient.CreateBranch(
		request.Pipeline.Name,
		pipelineInfo.OutputBranch,
		pipelineInfo.OutputBranch,
		provenance,
	); err != nil {
		return nil, err
	}

	if err := a.updatePipelineState(pachClient, request.Pipeline.Name, pps.PipelineState_PIPELINE_RUNNING); err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) StopPipeline(ctx context.Context, request *pps.StopPipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)

	// Get request.Pipeline's info
	pipelineInfo, err := a.inspectPipeline(pachClient, request.Pipeline.Name)
	if err != nil {
		return nil, err
	}

	// check if the caller is authorized to update this pipeline
	if err := a.authorizePipelineOp(pachClient, pipelineOpUpdate, pipelineInfo.Input, pipelineInfo.Pipeline.Name); err != nil {
		return nil, err
	}

	// Remove branch provenance (pass branch twice so that it continues to point
	// at the same commit, but also pass empty provenance slice)
	if err := pachClient.CreateBranch(
		request.Pipeline.Name,
		pipelineInfo.OutputBranch,
		pipelineInfo.OutputBranch,
		nil,
	); err != nil {
		return nil, err
	}

	// Update PipelineInfo with new state
	if err := a.updatePipelineState(pachClient, request.Pipeline.Name, pps.PipelineState_PIPELINE_PAUSED); err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) RerunPipeline(ctx context.Context, request *pps.RerunPipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())

	return nil, fmt.Errorf("TODO")
}

func (a *apiServer) DeleteAll(ctx context.Context, request *types.Empty) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)
	ctx = pachClient.Ctx() // pachClient will propagate auth info

	if me, err := pachClient.WhoAmI(ctx, &auth.WhoAmIRequest{}); err == nil {
		if !me.IsAdmin {
			return nil, fmt.Errorf("not authorized to delete all cluster data, must " +
				"be a cluster admin")
		}
	} else if !auth.IsNotActivatedError(err) {
		return nil, fmt.Errorf("could not verify that caller is admin: %v", err)
	}

	pipelineInfos, err := a.ListPipeline(ctx, &pps.ListPipelineRequest{})
	if err != nil {
		return nil, err
	}
	for _, pipelineInfo := range pipelineInfos.PipelineInfo {
		if _, err := a.DeletePipeline(ctx, &pps.DeletePipelineRequest{
			Pipeline: pipelineInfo.Pipeline,
		}); err != nil {
			return nil, err
		}
	}

	jobInfos, err := a.ListJob(ctx, &pps.ListJobRequest{})
	if err != nil {
		return nil, err
	}
	for _, jobInfo := range jobInfos.JobInfo {
		if _, err := a.DeleteJob(ctx, &pps.DeleteJobRequest{jobInfo.Job}); err != nil {
			return nil, err
		}
	}
	return &types.Empty{}, err
}

func (a *apiServer) GarbageCollect(ctx context.Context, request *pps.GarbageCollectRequest) (response *pps.GarbageCollectResponse, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	pachClient := a.getPachClient().WithCtx(ctx)
	ctx = pachClient.Ctx() // pachClient will propagate auth info
	pfsClient := pachClient.PfsAPIClient
	objClient := pachClient.ObjectAPIClient

	// The set of objects that are in use.
	activeObjects := make(map[string]bool)
	var activeObjectsMu sync.Mutex
	// A helper function for adding active objects in a thread-safe way
	addActiveObjects := func(objects ...*pfs.Object) {
		activeObjectsMu.Lock()
		defer activeObjectsMu.Unlock()
		for _, object := range objects {
			if object != nil {
				activeObjects[object.Hash] = true
			}
		}
	}
	// A helper function for adding objects that are actually hash trees,
	// which in turn contain active objects.
	addActiveTree := func(object *pfs.Object) error {
		if object == nil {
			return nil
		}
		addActiveObjects(object)
		getObjectClient, err := objClient.GetObject(ctx, object)
		if err != nil {
			return fmt.Errorf("error getting commit tree: %v", err)
		}

		var buf bytes.Buffer
		if err := grpcutil.WriteFromStreamingBytesClient(getObjectClient, &buf); err != nil {
			return fmt.Errorf("error reading commit tree: %v", err)
		}

		tree, err := hashtree.Deserialize(buf.Bytes())
		if err != nil {
			return err
		}

		return tree.Walk("/", func(path string, node *hashtree.NodeProto) error {
			if node.FileNode != nil {
				addActiveObjects(node.FileNode.Objects...)
			}
			return nil
		})
	}

	// Get all repos
	repoInfos, err := pfsClient.ListRepo(ctx, &pfs.ListRepoRequest{})
	if err != nil {
		return nil, err
	}

	// Get all commit trees
	limiter := limit.New(100)
	var eg errgroup.Group
	for _, repo := range repoInfos.RepoInfo {
		repo := repo
		client, err := pfsClient.ListCommitStream(ctx, &pfs.ListCommitRequest{
			Repo: repo.Repo,
		})
		if err != nil {
			return nil, err
		}
		for {
			commit, err := client.Recv()
			if err == io.EOF {
				break
			} else if err != nil {
				return nil, grpcutil.ScrubGRPC(err)
			}
			limiter.Acquire()
			eg.Go(func() error {
				defer limiter.Release()
				return addActiveTree(commit.Tree)
			})
		}
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// Get all objects referenced by pipeline tags
	pipelineInfos, err := a.ListPipeline(ctx, &pps.ListPipelineRequest{})
	if err != nil {
		return nil, err
	}

	// The set of tags that are active
	activeTags := make(map[string]bool)
	for _, pipelineInfo := range pipelineInfos.PipelineInfo {
		tags, err := objClient.ListTags(ctx, &pfs.ListTagsRequest{
			Prefix:        client.DatumTagPrefix(pipelineInfo.Salt),
			IncludeObject: true,
		})
		if err != nil {
			return nil, fmt.Errorf("error listing tagged objects: %v", err)
		}

		for resp, err := tags.Recv(); err != io.EOF; resp, err = tags.Recv() {
			resp := resp
			if err != nil {
				return nil, err
			}
			activeTags[resp.Tag] = true
			limiter.Acquire()
			eg.Go(func() error {
				defer limiter.Release()
				return addActiveTree(resp.Object)
			})
		}
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// Iterate through all objects.  If they are not active, delete them.
	objects, err := objClient.ListObjects(ctx, &pfs.ListObjectsRequest{})
	if err != nil {
		return nil, err
	}

	var objectsToDelete []*pfs.Object
	deleteObjectsIfMoreThan := func(n int) error {
		if len(objectsToDelete) > n {
			if _, err := objClient.DeleteObjects(ctx, &pfs.DeleteObjectsRequest{
				Objects: objectsToDelete,
			}); err != nil {
				return fmt.Errorf("error deleting objects: %v", err)
			}
			objectsToDelete = []*pfs.Object{}
		}
		return nil
	}
	for object, err := objects.Recv(); err != io.EOF; object, err = objects.Recv() {
		if err != nil {
			return nil, fmt.Errorf("error receiving objects from ListObjects: %v", err)
		}
		if !activeObjects[object.Hash] {
			objectsToDelete = append(objectsToDelete, object)
		}
		// Delete objects in batches
		if err := deleteObjectsIfMoreThan(100); err != nil {
			return nil, err
		}
	}
	if err := deleteObjectsIfMoreThan(0); err != nil {
		return nil, err
	}

	// Iterate through all tags.  If they are not active, delete them
	tags, err := objClient.ListTags(ctx, &pfs.ListTagsRequest{})
	if err != nil {
		return nil, err
	}
	var tagsToDelete []string
	deleteTagsIfMoreThan := func(n int) error {
		if len(tagsToDelete) > n {
			if _, err := objClient.DeleteTags(ctx, &pfs.DeleteTagsRequest{
				Tags: tagsToDelete,
			}); err != nil {
				return fmt.Errorf("error deleting tags: %v", err)
			}
			tagsToDelete = []string{}
		}
		return nil
	}
	for resp, err := tags.Recv(); err != io.EOF; resp, err = tags.Recv() {
		if err != nil {
			return nil, fmt.Errorf("error receiving tags from ListTags: %v", err)
		}
		if !activeTags[resp.Tag] {
			tagsToDelete = append(tagsToDelete, resp.Tag)
		}
		if err := deleteTagsIfMoreThan(100); err != nil {
			return nil, err
		}
	}
	if err := deleteTagsIfMoreThan(0); err != nil {
		return nil, err
	}

	if err := a.incrementGCGeneration(ctx); err != nil {
		return nil, err
	}

	return &pps.GarbageCollectResponse{}, nil
}

// incrementGCGeneration increments the GC generation number in etcd
func (a *apiServer) incrementGCGeneration(ctx context.Context) error {
	resp, err := a.etcdClient.Get(ctx, client.GCGenerationKey)
	if err != nil {
		return err
	}

	if resp.Count == 0 {
		// If the generation number does not exist, create it.
		// It's important that the new generation is 1, as the first
		// generation is assumed to be 0.
		if _, err := a.etcdClient.Put(ctx, client.GCGenerationKey, "1"); err != nil {
			return err
		}
	} else {
		oldGen, err := strconv.Atoi(string(resp.Kvs[0].Value))
		if err != nil {
			return err
		}
		newGen := oldGen + 1
		if _, err := a.etcdClient.Put(ctx, client.GCGenerationKey, strconv.Itoa(newGen)); err != nil {
			return err
		}
	}
	return nil
}

func isAlreadyExistsErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

func isNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// pipelineStateToStopped defines what pipeline states are "stopped"
// states, meaning that pipelines in this state should not be managed
// by pipelineManager
func pipelineStateToStopped(state pps.PipelineState) bool {
	switch state {
	case pps.PipelineState_PIPELINE_STARTING:
		return false
	case pps.PipelineState_PIPELINE_RUNNING:
		return false
	case pps.PipelineState_PIPELINE_RESTARTING:
		return false
	case pps.PipelineState_PIPELINE_PAUSED:
		return true
	case pps.PipelineState_PIPELINE_FAILURE:
		return true
	default:
		panic(fmt.Sprintf("unrecognized pipeline state: %s", state))
	}
}

func (a *apiServer) updatePipelineState(pachClient *client.APIClient, pipelineName string, state pps.PipelineState) error {
	_, err := col.NewSTM(pachClient.Ctx(), a.etcdClient, func(stm col.STM) error {
		pipelines := a.pipelines.ReadWrite(stm)
		pipelinePtr := &pps.EtcdPipelineInfo{}
		if err := pipelines.Get(pipelineName, pipelinePtr); err != nil {
			return err
		}
		pipelinePtr.State = state
		pipelines.Put(pipelineName, pipelinePtr)
		return nil
	})
	if isNotFoundErr(err) {
		return newErrPipelineNotFound(pipelineName)
	}
	return err
}

func (a *apiServer) updateJobState(stm col.STM, jobPtr *pps.EtcdJobInfo, state pps.JobState) error {
	pipelines := a.pipelines.ReadWrite(stm)
	pipelinePtr := &pps.EtcdPipelineInfo{}
	if err := pipelines.Get(jobPtr.Pipeline.Name, pipelinePtr); err != nil {
		return err
	}
	if pipelinePtr.JobCounts == nil {
		pipelinePtr.JobCounts = make(map[int32]int32)
	}
	if pipelinePtr.JobCounts[int32(jobPtr.State)] != 0 {
		pipelinePtr.JobCounts[int32(jobPtr.State)]--
	}
	pipelinePtr.JobCounts[int32(state)]++
	pipelines.Put(jobPtr.Pipeline.Name, pipelinePtr)
	jobPtr.State = state
	jobs := a.jobs.ReadWrite(stm)
	jobs.Put(jobPtr.Job.ID, jobPtr)
	return nil
}

func (a *apiServer) getPachClient() *client.APIClient {
	a.pachClientOnce.Do(func() {
		var err error
		a.pachClient, err = client.NewFromAddress(a.address)
		if err != nil {
			panic(fmt.Sprintf("pps failed to initialize pach client: %v", err))
		}
		// Initialize spec repo
		if err := a.pachClient.CreateRepo(ppsconsts.SpecRepo); err != nil {
			if !isAlreadyExistsErr(err) {
				panic(fmt.Sprintf("could not create pipeline spec repo: %v", err))
			}
		}
	})
	return a.pachClient
}

// RepoNameToEnvString is a helper which uppercases a repo name for
// use in environment variable names.
func RepoNameToEnvString(repoName string) string {
	return strings.ToUpper(repoName)
}

func (a *apiServer) rcPods(rcName string) ([]v1.Pod, error) {
	podList, err := a.kubeClient.CoreV1().Pods(a.namespace).List(metav1.ListOptions{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ListOptions",
			APIVersion: "v1",
		},
		LabelSelector: metav1.FormatLabelSelector(metav1.SetAsLabelSelector(labels(rcName))),
	})
	if err != nil {
		return nil, err
	}
	return podList.Items, nil
}

func (a *apiServer) resolveCommit(pachClient *client.APIClient, commit *pfs.Commit) (*pfs.Commit, error) {
	ci, err := pachClient.InspectCommit(commit.Repo.Name, commit.ID)
	if err != nil {
		return nil, err
	}
	return ci.Commit, nil
}

func labels(app string) map[string]string {
	return map[string]string{
		"app":       app,
		"suite":     suite,
		"component": "worker",
	}
}

type podSlice []v1.Pod

func (s podSlice) Len() int {
	return len(s)
}
func (s podSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s podSlice) Less(i, j int) bool {
	return s[i].ObjectMeta.Name < s[j].ObjectMeta.Name
}

func now() *types.Timestamp {
	t, err := types.TimestampProto(time.Now())
	if err != nil {
		panic(err)
	}
	return t
}
