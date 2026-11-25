package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type modelTestMemoryRepo struct {
	mu                      sync.Mutex
	results                 map[int64]map[int64]AccountModelTestJob
	pending                 map[string]accountModelTestRuntime
	cleared                 map[int64]time.Time
	failSave                bool
	failRead                bool
	ownerLost               bool
	waitForSaveCancellation bool
	saveCalls               int
}

func newModelTestMemoryRepo() *modelTestMemoryRepo {
	return &modelTestMemoryRepo{results: map[int64]map[int64]AccountModelTestJob{}, pending: map[string]accountModelTestRuntime{}, cleared: map[int64]time.Time{}}
}

func (r *modelTestMemoryRepo) RecoverInterrupted(context.Context) error { return nil }
func (r *modelTestMemoryRepo) Close() error                             { return nil }
func (r *modelTestMemoryRepo) CheckOwner(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ownerLost {
		return errors.New("owner lost")
	}
	return nil
}
func (r *modelTestMemoryRepo) PutPending(_ context.Context, adminID int64, job AccountModelTestJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending[job.ID] = accountModelTestRuntime{adminID: adminID, job: job}
	return nil
}
func (r *modelTestMemoryRepo) SaveResults(ctx context.Context, adminID int64, jobs []AccountModelTestJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveCalls++
	if r.waitForSaveCancellation {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.failSave {
		return errors.New("write unavailable")
	}
	if r.results[adminID] == nil {
		r.results[adminID] = map[int64]AccountModelTestJob{}
	}
	for _, job := range jobs {
		if job.CreatedAt.After(r.cleared[adminID]) {
			old, ok := r.results[adminID][job.AccountID]
			if !ok || job.CreatedAt.After(old.CreatedAt) || job.ID == old.ID {
				job.Persisted = true
				r.results[adminID][job.AccountID] = job
			}
		}
		delete(r.pending, job.ID)
	}
	return nil
}
func (r *modelTestMemoryRepo) ListResults(_ context.Context, adminID int64) (*AccountModelTestResults, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failRead {
		return nil, errors.New("read unavailable")
	}
	out := &AccountModelTestResults{Results: []AccountModelTestJob{}}
	for _, job := range r.results[adminID] {
		out.Results = append(out.Results, job)
	}
	if at, ok := r.cleared[adminID]; ok {
		out.ClearedAt = &at
	}
	return out, nil
}
func (r *modelTestMemoryRepo) Clear(_ context.Context, adminID int64, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared[adminID] = at
	delete(r.results, adminID)
	for id, p := range r.pending {
		if p.adminID == adminID {
			delete(r.pending, id)
		}
	}
	return nil
}

type modelTestAccountReader struct{}

func (modelTestAccountReader) GetByID(_ context.Context, id int64) (*Account, error) {
	if id <= 0 {
		return nil, errors.New("not found")
	}
	return &Account{ID: id, Name: fmt.Sprintf("账号%d", id), Platform: PlatformOpenAI}, nil
}

type modelTestExecutorFunc func(context.Context, int64, string, string, string) (*ScheduledTestResult, error)

func (f modelTestExecutorFunc) RunModelTestBackground(ctx context.Context, id int64, model, prompt, reasoning string) (*ScheduledTestResult, error) {
	return f(ctx, id, model, prompt, reasoning)
}

func modelTestInput(id int64) AccountModelTestInput {
	return AccountModelTestInput{AccountID: id, ModelID: "test-text-model", Prompt: "题目", PromptKey: "custom", ReasoningEffort: "high"}
}
func newModelTestService(t *testing.T, repo *modelTestMemoryRepo, fn modelTestExecutorFunc) *AccountModelTestService {
	t.Helper()
	svc := NewAccountModelTestService(repo, modelTestAccountReader{}, fn)
	t.Cleanup(svc.Stop)
	return svc
}
func waitModelTestIdle(t *testing.T, svc *AccountModelTestService) {
	t.Helper()
	require.Eventually(t, func() bool { svc.mu.Lock(); defer svc.mu.Unlock(); return len(svc.active) == 0 }, 3*time.Second, 5*time.Millisecond)
}

func TestAccountModelTestCreateIsolationAndArguments(t *testing.T) {
	repo := newModelTestMemoryRepo()
	started := make(chan AccountModelTestInput, 1)
	release := make(chan struct{})
	svc := newModelTestService(t, repo, func(ctx context.Context, id int64, model, prompt, reasoning string) (*ScheduledTestResult, error) {
		started <- AccountModelTestInput{AccountID: id, ModelID: model, Prompt: prompt, ReasoningEffort: reasoning}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &ScheduledTestResult{Status: "success", ResponseText: "回答"}, nil
	})
	job, err := svc.Create(context.Background(), 11, modelTestInput(7))
	require.NoError(t, err)
	select {
	case in := <-started:
		require.Equal(t, "high", in.ReasoningEffort)
		require.Equal(t, "题目", in.Prompt)
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	_, err = svc.Create(context.Background(), 12, modelTestInput(7))
	require.ErrorIs(t, err, ErrModelTestBusy)
	_, err = svc.Cancel(12, job.ID)
	require.ErrorIs(t, err, ErrModelTestNotFound)
	other, err := svc.ListJobs(context.Background(), 12)
	require.NoError(t, err)
	require.Empty(t, other)
	close(release)
	waitModelTestIdle(t, svc)
	results, err := svc.ListResults(context.Background(), 11)
	require.NoError(t, err)
	require.Len(t, results.Results, 1)
	require.Equal(t, "success", results.Results[0].Status)
	require.True(t, results.Results[0].Persisted)
}

func TestAccountModelTestConcurrencyAndQueuedCancellation(t *testing.T) {
	repo := newModelTestMemoryRepo()
	started := make(chan int64, 10)
	release := make(chan struct{})
	var running, peak atomic.Int32
	svc := newModelTestService(t, repo, func(ctx context.Context, id int64, _, _, _ string) (*ScheduledTestResult, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		started <- id
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &ScheduledTestResult{Status: "success", ResponseText: "ok"}, nil
	})
	for id := int64(1); id <= 7; id++ {
		_, err := svc.Create(context.Background(), 1, modelTestInput(id))
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("five workers did not start")
		}
	}
	jobs, err := svc.ListJobs(context.Background(), 1)
	require.NoError(t, err)
	var queued AccountModelTestJob
	for _, job := range jobs {
		if job.Status == "queued" {
			queued = job
			break
		}
	}
	require.NotEmpty(t, queued.ID)
	_, err = svc.Cancel(1, queued.ID)
	require.NoError(t, err)
	close(release)
	waitModelTestIdle(t, svc)
	require.LessOrEqual(t, peak.Load(), int32(5))
	close(started)
	for id := range started {
		require.NotEqual(t, queued.AccountID, id)
	}
}

func TestAccountModelTestClearCancelsAndBlocksOldImport(t *testing.T) {
	repo := newModelTestMemoryRepo()
	started := make(chan struct{})
	release := make(chan struct{})
	svc := newModelTestService(t, repo, func(context.Context, int64, string, string, string) (*ScheduledTestResult, error) {
		close(started)
		<-release
		return &ScheduledTestResult{Status: "success", ResponseText: "旧回答"}, nil
	})
	job, err := svc.Create(context.Background(), 1, modelTestInput(9))
	require.NoError(t, err)
	<-started
	watermark, err := svc.Clear(context.Background(), 1)
	require.NoError(t, err)
	close(release)
	waitModelTestIdle(t, svc)
	data, err := svc.ListResults(context.Background(), 1)
	require.NoError(t, err)
	require.Empty(t, data.Results)
	require.Equal(t, *watermark, *data.ClearedAt)
	finished := time.Now()
	job.Status = "success"
	job.FinishedAt = &finished
	job.ResponseText = "旧导入"
	data, err = svc.Import(context.Background(), 1, []AccountModelTestJob{*job})
	require.NoError(t, err)
	require.Empty(t, data.Results)
}

func TestAccountModelTestCancellationPreservesPartialResponse(t *testing.T) {
	repo := newModelTestMemoryRepo()
	started := make(chan struct{})
	svc := newModelTestService(t, repo, func(ctx context.Context, _ int64, _, _, _ string) (*ScheduledTestResult, error) {
		close(started)
		<-ctx.Done()
		return &ScheduledTestResult{Status: "failed", ResponseText: "部分回答"}, ctx.Err()
	})
	job, err := svc.Create(context.Background(), 1, modelTestInput(4))
	require.NoError(t, err)
	<-started
	cancelled, err := svc.Cancel(1, job.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", cancelled.Status)
	waitModelTestIdle(t, svc)
	data, err := svc.ListResults(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, data.Results, 1)
	require.Equal(t, "cancelled", data.Results[0].Status)
	require.Equal(t, "部分回答", data.Results[0].ResponseText)
}

func TestAccountModelTestFallbackAndLatestPerAccount(t *testing.T) {
	repo := newModelTestMemoryRepo()
	repo.failSave = true
	repo.failRead = true
	svc := newModelTestService(t, repo, func(context.Context, int64, string, string, string) (*ScheduledTestResult, error) {
		return &ScheduledTestResult{Status: "success", ResponseText: "可导出回答"}, nil
	})
	for i := 0; i < 3; i++ {
		_, err := svc.Create(context.Background(), 1, modelTestInput(8))
		require.NoError(t, err)
		waitModelTestIdle(t, svc)
	}
	data, err := svc.ListResults(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, data.Results, 1)
	require.False(t, data.Results[0].Persisted)
	require.Equal(t, "可导出回答", data.Results[0].ResponseText)
	svc.mu.Lock()
	require.Len(t, svc.recent, 1)
	svc.mu.Unlock()
}

func TestAccountModelTestTimeoutAndSafeBusinessFailure(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		repo := newModelTestMemoryRepo()
		svc := newModelTestService(t, repo, func(ctx context.Context, _ int64, _, _, _ string) (*ScheduledTestResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		svc.timeout = 20 * time.Millisecond
		_, err := svc.Create(context.Background(), 1, modelTestInput(3))
		require.NoError(t, err)
		waitModelTestIdle(t, svc)
		data, err := svc.ListResults(context.Background(), 1)
		require.NoError(t, err)
		require.Equal(t, "failed", data.Results[0].Status)
		require.Contains(t, data.Results[0].ErrorMessage, "超过")
	})
	t.Run("business", func(t *testing.T) {
		repo := newModelTestMemoryRepo()
		svc := newModelTestService(t, repo, func(context.Context, int64, string, string, string) (*ScheduledTestResult, error) {
			return &ScheduledTestResult{Status: "failed", ErrorMessage: "此协议不支持 reasoning"}, errors.New("safe validation")
		})
		_, err := svc.Create(context.Background(), 1, modelTestInput(3))
		require.NoError(t, err)
		waitModelTestIdle(t, svc)
		data, err := svc.ListResults(context.Background(), 1)
		require.NoError(t, err)
		require.Equal(t, "此协议不支持 reasoning", data.Results[0].ErrorMessage)
	})
}

func TestAccountModelTestLostOwnerCancelsAndRejectsNewJobs(t *testing.T) {
	repo := newModelTestMemoryRepo()
	started := make(chan struct{})
	svc := newModelTestService(t, repo, func(ctx context.Context, _ int64, _, _, _ string) (*ScheduledTestResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	_, err := svc.Create(context.Background(), 1, modelTestInput(1))
	require.NoError(t, err)
	<-started
	repo.mu.Lock()
	repo.ownerLost = true
	repo.mu.Unlock()
	_, err = svc.Create(context.Background(), 1, modelTestInput(2))
	require.ErrorIs(t, err, ErrModelTestUnavailable)
	waitModelTestIdle(t, svc)
	data, err := svc.ListResults(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, "interrupted", data.Results[0].Status)
	require.False(t, data.Results[0].Persisted)
}

func TestAccountModelTestSVGAndValidation(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{{"<svg><path d=\"M0 0 L1 1\"/></svg>", true}, {"说明\n```svg\n<svg><circle r=\"3\"/></svg>\n```", true}, {"<svg></svg>", false}, {"<svg><metadata>none</metadata></svg>", false}, {"<svg><script>alert(1)</script></svg>", false}, {"<svg><path></svg>", false}, {"拒绝回答", false}, {"<svg><path/></svg><svg><path/></svg>", false}} {
		require.Equal(t, test.want, validModelTestSVG(test.value), test.value)
	}
	for _, model := range []string{"gpt-image-1", "whisper-1", "text-embedding-3-large", "sora-2", "bad\nmodel"} {
		in := modelTestInput(1)
		in.ModelID = model
		require.Error(t, validateAccountModelTestInput(&in))
	}
	in := modelTestInput(1)
	in.Prompt = strings.Repeat("x", AccountModelTestMaxPrompt+1)
	require.Error(t, validateAccountModelTestInput(&in))
}

func TestAccountModelTestRecentPrunesByAdministratorAndAccount(t *testing.T) {
	repo := newModelTestMemoryRepo()
	svc := newModelTestService(t, repo, func(context.Context, int64, string, string, string) (*ScheduledTestResult, error) { return nil, nil })
	svc.mu.Lock()
	defer svc.mu.Unlock()
	for i := 0; i < 502; i++ {
		at := time.Now().Add(time.Duration(i) * time.Millisecond)
		r := &accountModelTestRuntime{adminID: 1, job: AccountModelTestJob{ID: uuid.NewString(), AccountID: int64(i), CreatedAt: at, Status: "success", ResponseText: "ok"}}
		svc.persistAndRemember(r)
	}
	require.Len(t, svc.recent, 500)
	ids := []int{}
	for _, r := range svc.recent {
		ids = append(ids, int(r.job.AccountID))
	}
	sort.Ints(ids)
	require.Equal(t, 2, ids[0])
}

func TestAccountModelTestExecutorPanicIsIsolated(t *testing.T) {
	repo := newModelTestMemoryRepo()
	svc := newModelTestService(t, repo, func(context.Context, int64, string, string, string) (*ScheduledTestResult, error) {
		panic("secret must not leak")
	})
	_, err := svc.Create(context.Background(), 1, modelTestInput(1))
	require.NoError(t, err)
	waitModelTestIdle(t, svc)
	data, err := svc.ListResults(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, "failed", data.Results[0].Status)
	require.Contains(t, data.Results[0].ErrorMessage, "内部异常")
	require.NotContains(t, data.Results[0].ErrorMessage, "secret")
}

func TestAccountModelTestShutdownCancelsAllBeforeBoundedBatchSave(t *testing.T) {
	repo := newModelTestMemoryRepo()
	started := make(chan struct{}, 5)
	cancelled := make(chan struct{}, 5)
	svc := newModelTestService(t, repo, func(ctx context.Context, _ int64, _, _, _ string) (*ScheduledTestResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		return nil, ctx.Err()
	})
	svc.shutdownTimeout = 30 * time.Millisecond
	for id := int64(1); id <= 8; id++ {
		_, err := svc.Create(context.Background(), 1, modelTestInput(id))
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		<-started
	}
	repo.mu.Lock()
	repo.waitForSaveCancellation = true
	repo.mu.Unlock()
	begin := time.Now()
	svc.Stop()
	require.Less(t, time.Since(begin), time.Second)
	require.Len(t, cancelled, 5)
	waitModelTestIdle(t, svc)
	repo.mu.Lock()
	require.Equal(t, 1, repo.saveCalls)
	repo.mu.Unlock()
}
