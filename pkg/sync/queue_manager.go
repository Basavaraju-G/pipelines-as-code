package sync

import (
	"context"
	"fmt"
	"sync"

	"github.com/openshift-pipelines/pipelines-as-code/pkg/apis/pipelinesascode"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/apis/pipelinesascode/v1alpha1"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/generated/clientset/versioned"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/kubeinteraction"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	versioned2 "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	"go.uber.org/zap"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type QueueManager struct {
	queueMap map[string]Semaphore
	lock     *sync.Mutex
	logger   *zap.SugaredLogger
}

func NewQueueManager(logger *zap.SugaredLogger) *QueueManager {
	return &QueueManager{
		queueMap: make(map[string]Semaphore),
		lock:     &sync.Mutex{},
		logger:   logger,
	}
}

// getSemaphore returns existing semaphore created for repository or create
// a new one with limit provided in repository
// Semaphore: nothing but a waiting and a running queue for a repository
// with limit deciding how many should be running at a time
func (qm *QueueManager) getSemaphore(repo *v1alpha1.Repository) (Semaphore, error) {
	repoKey := repoKey(repo)

	if sema, found := qm.queueMap[repoKey]; found {
		if err := qm.checkAndUpdateSemaphoreSize(repo, sema); err != nil {
			return nil, err
		}
		return sema, nil
	}

	// create a new semaphore
	qm.queueMap[repoKey] = newSemaphore(repoKey, *repo.Spec.ConcurrencyLimit)

	return qm.queueMap[repoKey], nil
}

func repoKey(repo *v1alpha1.Repository) string {
	return fmt.Sprintf("%s/%s", repo.Namespace, repo.Name)
}

func (qm *QueueManager) checkAndUpdateSemaphoreSize(repo *v1alpha1.Repository, semaphore Semaphore) error {
	limit := *repo.Spec.ConcurrencyLimit
	if limit != semaphore.getLimit() {
		if semaphore.resize(limit) {
			return nil
		}
		return fmt.Errorf("failed to resize semaphore")
	}
	return nil
}

// AddToQueue adds the pipelineRun to the waiting queue of the repository
// and if it is at the top and ready to run which means currently running pipelineRun < limit
// then move it to running queue
func (qm *QueueManager) AddToQueue(repo *v1alpha1.Repository, run *v1beta1.PipelineRun) (bool, string, error) {
	qm.lock.Lock()
	defer qm.lock.Unlock()

	sema, err := qm.getSemaphore(repo)
	if err != nil {
		return false, "", err
	}

	qKey := getQueueKey(run)
	sema.addToQueue(qKey, run.CreationTimestamp.Time)

	qm.logger.Infof("added pipelineRun (%s) to queue for repository (%s)", qKey, repoKey(repo))

	acquired, msg := sema.tryAcquire(qKey)
	if acquired {
		qm.logger.Infof("moved (%s) to running for repository (%s)", qKey, repoKey(repo))
	}
	return acquired, msg, nil
}

// RemoveFromQueue removes the pipelineRun from the queues of the repository
// It also start the next one which is on top of the waiting queue and return its name
// if started or returns ""
func (qm *QueueManager) RemoveFromQueue(repo *v1alpha1.Repository, run *v1beta1.PipelineRun) string {
	qm.lock.Lock()
	defer qm.lock.Unlock()

	repoKey := repoKey(repo)
	sema, found := qm.queueMap[repoKey]
	if !found {
		return ""
	}

	qKey := getQueueKey(run)
	sema.release(qKey)
	sema.removeFromQueue(qKey)
	qm.logger.Infof("removed (%s) for repository (%s)", qKey, repoKey)

	if next := sema.acquireLatest(); next != "" {
		qm.logger.Infof("moved (%s) to running for repository (%s)", qKey, repoKey)
		return next
	}
	return ""
}

func getQueueKey(run *v1beta1.PipelineRun) string {
	return fmt.Sprintf("%s/%s", run.Namespace, run.Name)
}

// InitQueues rebuild all the queues for all repository if concurrency is defined before
// reconciler started reconciling them
func (qm *QueueManager) InitQueues(ctx context.Context, tekton versioned2.Interface, pac versioned.Interface) error {
	// fetch all repos
	repos, err := pac.PipelinesascodeV1alpha1().Repositories("").List(ctx, v1.ListOptions{})
	if err != nil {
		return err
	}

	// pipelineRuns from the namespace where repository is present
	// those are required for creating queues
	for _, repo := range repos.Items {
		repo := repo
		if repo.Spec.ConcurrencyLimit == nil || *repo.Spec.ConcurrencyLimit == 0 {
			continue
		}

		// add all pipelineRuns in queued state to pending queue
		prs, err := tekton.TektonV1beta1().PipelineRuns(repo.Namespace).
			List(ctx, v1.ListOptions{
				LabelSelector: fmt.Sprintf("%s/%s=%s", pipelinesascode.GroupName, "state", kubeinteraction.StateQueued),
			})
		if err != nil {
			return err
		}

		for _, pr := range prs.Items {
			pr := pr
			sema, err := qm.getSemaphore(&repo)
			if err != nil {
				return err
			}

			qKey := getQueueKey(&pr)
			sema.addToQueue(qKey, pr.CreationTimestamp.Time)
		}

		// now fetch all started pipelineRun and update the running queue
		prs, err = tekton.TektonV1beta1().PipelineRuns(repo.Namespace).
			List(ctx, v1.ListOptions{
				LabelSelector: fmt.Sprintf("%s/%s=%s", pipelinesascode.GroupName, "state", kubeinteraction.StateStarted),
			})
		if err != nil {
			return err
		}

		for _, pr := range prs.Items {
			pr := pr
			sema, err := qm.getSemaphore(&repo)
			if err != nil {
				return err
			}
			sema.acquire(getQueueKey(&pr))
		}
	}

	return nil
}

func (qm *QueueManager) RemoveRepository(repo *v1alpha1.Repository) {
	qm.lock.Lock()
	defer qm.lock.Unlock()

	repoKey := repoKey(repo)
	delete(qm.queueMap, repoKey)
}

func (qm *QueueManager) QueuedPipelineRuns(repo *v1alpha1.Repository) []string {
	qm.lock.Lock()
	defer qm.lock.Unlock()

	repoKey := repoKey(repo)
	if sema, ok := qm.queueMap[repoKey]; ok {
		return sema.getCurrentPending()
	}
	return []string{}
}

func (qm *QueueManager) RunningPipelineRuns(repo *v1alpha1.Repository) []string {
	qm.lock.Lock()
	defer qm.lock.Unlock()

	repoKey := repoKey(repo)
	if sema, ok := qm.queueMap[repoKey]; ok {
		return sema.getCurrentRunning()
	}
	return []string{}
}
