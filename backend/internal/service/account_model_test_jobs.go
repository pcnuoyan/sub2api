package service

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	AccountModelTestMaxResults  = 500
	AccountModelTestMaxPrompt   = 16384
	AccountModelTestMaxResponse = 131072
	accountModelTestMaxPending  = 100
	accountModelTestMaxRecent   = 1000
	accountModelTestRecentBytes = 32 << 20
)

var (
	ErrModelTestNotFound    = errors.New("测试任务不存在")
	ErrModelTestBusy        = errors.New("该账号已有测试任务，或测试队列已满")
	ErrModelTestUnavailable = errors.New("测试任务存储不可用，请稍后重试")
)

// AccountModelTestJob 是管理员独立的测试请求及结果快照，不包含账号凭据。
type AccountModelTestJob struct {
	ID              string     `json:"id"`
	AccountID       int64      `json:"account_id"`
	AccountName     string     `json:"account_name"`
	Platform        string     `json:"platform"`
	ModelID         string     `json:"model_id"`
	Prompt          string     `json:"prompt"`
	PromptKey       string     `json:"prompt_key"`
	ReasoningEffort string     `json:"reasoning_effort"`
	Status          string     `json:"status"`
	ResponseText    string     `json:"response_text"`
	ErrorMessage    string     `json:"error_message"`
	LatencyMs       int64      `json:"latency_ms"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	Persisted       bool       `json:"persisted"`
}

type AccountModelTestInput struct {
	AccountID       int64  `json:"account_id"`
	ModelID         string `json:"model_id"`
	Prompt          string `json:"prompt"`
	PromptKey       string `json:"prompt_key"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type AccountModelTestResults struct {
	Results   []AccountModelTestJob `json:"results"`
	ClearedAt *time.Time            `json:"cleared_at"`
}

// AccountModelTestRepository 的写入操作必须在管理员清空水位锁内完成。
type AccountModelTestRepository interface {
	RecoverInterrupted(context.Context) error
	PutPending(context.Context, int64, AccountModelTestJob) error
	SaveResults(context.Context, int64, []AccountModelTestJob) error
	ListResults(context.Context, int64) (*AccountModelTestResults, error)
	Clear(context.Context, int64, time.Time) error
	CheckOwner(context.Context) error
	Close() error
}

type AccountModelTestExecutor interface {
	RunModelTestBackground(context.Context, int64, string, string, string) (*ScheduledTestResult, error)
}

type accountModelTestAccountReader interface {
	GetByID(context.Context, int64) (*Account, error)
}

type accountModelTestRuntime struct {
	adminID int64
	job     AccountModelTestJob
	cancel  context.CancelFunc
	hidden  bool
}

// AccountModelTestService 以单实例执行队列隔离管理员任务；数据库保存待执行请求和最终结果。
// 数据库会话锁只允许一个实例启用执行器；其他实例返回不可用，不恢复或抢占在途任务。
type AccountModelTestService struct {
	repo            AccountModelTestRepository
	accounts        accountModelTestAccountReader
	executor        AccountModelTestExecutor
	mu              sync.Mutex
	active          map[string]*accountModelTestRuntime
	recent          map[string]*accountModelTestRuntime
	cleared         map[int64]time.Time
	sem             chan struct{}
	timeout         time.Duration
	shutdownTimeout time.Duration
	initOnce        sync.Once
	initErr         error
	stopped         bool
	ownerLost       bool
	stopCh          chan struct{}
	stopOnce        sync.Once
	wg              sync.WaitGroup
}

func NewAccountModelTestService(repo AccountModelTestRepository, accounts accountModelTestAccountReader, executor AccountModelTestExecutor) *AccountModelTestService {
	return &AccountModelTestService{repo: repo, accounts: accounts, executor: executor, active: make(map[string]*accountModelTestRuntime), recent: make(map[string]*accountModelTestRuntime), cleared: make(map[int64]time.Time), sem: make(chan struct{}, 5), timeout: 20 * time.Minute, shutdownTimeout: 5 * time.Second, stopCh: make(chan struct{})}
}

// Start 只恢复状态，不重新执行重启前的付费请求。
func (s *AccountModelTestService) Start() error {
	s.initOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s.initErr = s.repo.RecoverInterrupted(ctx)
		if s.initErr == nil {
			go s.monitorOwner()
		}
	})
	return s.initErr
}

func validateAccountModelTestInput(in *AccountModelTestInput) error {
	in.ModelID = strings.TrimSpace(in.ModelID)
	in.Prompt = strings.TrimSpace(in.Prompt)
	in.ReasoningEffort = strings.TrimSpace(in.ReasoningEffort)
	if in.AccountID <= 0 || in.ModelID == "" || len(in.ModelID) > 200 || strings.IndexFunc(in.ModelID, unicode.IsSpace) >= 0 || strings.IndexFunc(in.ModelID, unicode.IsControl) >= 0 {
		return errors.New("账号或模型无效，模型名称最多 200 字节")
	}
	if in.Prompt == "" || len(in.Prompt) > AccountModelTestMaxPrompt {
		return errors.New("测试题不能为空且不能超过 16 KiB")
	}
	switch in.PromptKey {
	case "candy", "pelican", "custom":
	default:
		return errors.New("不支持的题目类型")
	}
	switch in.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return errors.New("不支持的 reasoning 参数")
	}
	model := strings.ToLower(in.ModelID)
	for _, marker := range []string{"image", "imagen", "dall-e", "video", "veo", "sora", "audio", "tts", "whisper", "transcrib", "embedding", "rerank"} {
		if strings.Contains(model, marker) {
			return errors.New("此页面仅支持文本生成模型")
		}
	}
	return nil
}

func (s *AccountModelTestService) Create(ctx context.Context, adminID int64, in AccountModelTestInput) (*AccountModelTestJob, error) {
	if adminID <= 0 {
		return nil, errors.New("需要管理员身份")
	}
	if err := validateAccountModelTestInput(&in); err != nil {
		return nil, err
	}
	if err := s.Start(); err != nil {
		return nil, ErrModelTestUnavailable
	}
	account, err := s.accounts.GetByID(ctx, in.AccountID)
	if err != nil || account == nil {
		return nil, errors.New("账号不存在")
	}
	mapped := in
	mapped.ModelID = account.GetMappedModel(in.ModelID)
	if err := validateAccountModelTestInput(&mapped); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, ErrModelTestUnavailable
	}
	if err := s.repo.CheckOwner(ctx); err != nil {
		s.loseOwnerLocked()
		return nil, ErrModelTestUnavailable
	}
	if len(s.active) >= accountModelTestMaxPending {
		return nil, ErrModelTestBusy
	}
	for _, r := range s.active {
		if r.job.AccountID == in.AccountID {
			return nil, ErrModelTestBusy
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	job := AccountModelTestJob{ID: uuid.NewString(), AccountID: in.AccountID, AccountName: account.Name, Platform: account.Platform, ModelID: in.ModelID, Prompt: in.Prompt, PromptKey: in.PromptKey, ReasoningEffort: in.ReasoningEffort, Status: "queued", CreatedAt: now, Persisted: true}
	if err := s.repo.PutPending(ctx, adminID, job); err != nil {
		return nil, ErrModelTestUnavailable
	}
	// 任务生命周期独立于创建请求；只有取消接口、清空或服务退出会取消它。
	taskCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
	runtime := &accountModelTestRuntime{adminID: adminID, job: job, cancel: cancel}
	s.active[job.ID] = runtime
	s.wg.Add(1)
	go s.run(taskCtx, runtime)
	return &job, nil
}

func (s *AccountModelTestService) run(ctx context.Context, r *accountModelTestRuntime) {
	defer s.wg.Done()
	defer r.cancel()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.finish(r, nil, ctx.Err())
		return
	}
	s.mu.Lock()
	if r.job.Status != "queued" || ctx.Err() != nil {
		s.mu.Unlock()
		s.finish(r, nil, ctx.Err())
		return
	}
	now := time.Now().UTC()
	r.job.Status = "running"
	r.job.StartedAt = &now
	persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.repo.CheckOwner(persistCtx); err != nil {
		cancel()
		s.loseOwnerLocked()
		s.mu.Unlock()
		s.finish(r, nil, context.Canceled)
		return
	}
	r.job.Persisted = s.repo.PutPending(persistCtx, r.adminID, r.job) == nil
	cancel()
	job := r.job
	s.mu.Unlock()
	result, err := s.execute(ctx, job)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	s.finish(r, result, err)
}

// execute 将单次测试内部异常限制在该任务，不自动重试或记录可能含凭据的 panic 值。
func (s *AccountModelTestService) execute(ctx context.Context, job AccountModelTestJob) (result *ScheduledTestResult, err error) {
	defer func() {
		if recover() != nil {
			result = &ScheduledTestResult{Status: "failed", ErrorMessage: "模型测试发生内部异常，请检查配置后重试"}
			err = nil
		}
	}()
	return s.executor.RunModelTestBackground(ctx, job.AccountID, job.ModelID, job.Prompt, job.ReasoningEffort)
}

func (s *AccountModelTestService) finish(r *accountModelTestRuntime, result *ScheduledTestResult, runErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer delete(s.active, r.job.ID)
	if terminalAccountModelTestStatus(r.job.Status) {
		// 用户取消立即呈现终态，外呼随后返回的部分内容仍可保存，但不能改回成功。
		if result != nil && result.ResponseText != "" && !r.hidden {
			r.job.ResponseText = truncateModelTestText(result.ResponseText, AccountModelTestMaxResponse)
			if s.stopped {
				r.job.Persisted = false
				s.rememberResult(r)
			} else {
				s.persistAndRemember(r)
			}
		}
		return
	}
	now := time.Now().UTC()
	r.job.FinishedAt = &now
	r.job.Status = "failed"
	if r.job.StartedAt != nil {
		r.job.LatencyMs = now.Sub(*r.job.StartedAt).Milliseconds()
	}
	if result != nil {
		r.job.ResponseText = result.ResponseText
		r.job.ErrorMessage = result.ErrorMessage
		if result.Status == "success" && strings.TrimSpace(result.ResponseText) != "" {
			r.job.Status = "success"
		}
	}
	if runErr != nil {
		r.job.Status = "failed"
		if r.job.ErrorMessage == "" {
			r.job.ErrorMessage = "模型请求失败，请检查账号、模型和代理配置"
		}
	}
	if errors.Is(runErr, context.Canceled) {
		r.job.Status = "cancelled"
		r.job.ErrorMessage = "测试已取消"
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		r.job.Status = "failed"
		r.job.ErrorMessage = "测试超过 20 分钟，已取消上游请求"
	}
	if len(r.job.ResponseText) > AccountModelTestMaxResponse {
		r.job.ResponseText = truncateModelTestText(r.job.ResponseText, AccountModelTestMaxResponse)
		r.job.Status = "failed"
		r.job.ErrorMessage = "回答超过 128 KiB，已截断，请导出保留内容"
	}
	r.job.ErrorMessage = truncateModelTestText(r.job.ErrorMessage, 8192)
	if r.job.Status == "success" && r.job.PromptKey == "pelican" && !validModelTestSVG(r.job.ResponseText) {
		r.job.Status = "failed"
		r.job.ErrorMessage = "回答没有完整有效的 SVG，请查看原始回答"
	}
	if r.job.Status == "failed" && r.job.ErrorMessage == "" {
		r.job.ErrorMessage = "模型未返回完整有效的回答"
	}
	if r.hidden {
		return
	}
	s.persistAndRemember(r)
}

func truncateModelTestText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// validModelTestSVG 仅校验完整 XML；展示端仍须隔离脚本和外部资源。
func validModelTestSVG(answer string) bool {
	start := strings.Index(answer, "<svg")
	end := strings.LastIndex(answer, "</svg>")
	if start < 0 || end < start {
		return false
	}
	decoder := xml.NewDecoder(strings.NewReader(answer[start : end+6]))
	depth := 0
	seen := false
	drawable := false
	ignoredDepth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return seen && drawable && depth == 0
		}
		if err != nil {
			return false
		}
		switch element := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				if seen || element.Name.Local != "svg" {
					return false
				}
				seen = true
			}
			depth++
			if ignoredDepth == 0 {
				switch element.Name.Local {
				case "script", "style", "metadata", "defs", "symbol", "clipPath", "mask", "marker", "pattern":
					ignoredDepth = depth
				case "path", "rect", "circle", "ellipse", "line", "polyline", "polygon", "text", "use":
					drawable = true
				}
			}
		case xml.EndElement:
			if ignoredDepth == depth {
				ignoredDepth = 0
			}
			depth--
		case xml.Directive:
			return false
		}
	}
}

func terminalAccountModelTestStatus(status string) bool {
	switch status {
	case "success", "failed", "cancelled", "interrupted":
		return true
	}
	return false
}

// persistAndRemember 在持有 mu 时调用，保留有界的可导出内存结果以应对入库故障。
func (s *AccountModelTestService) persistAndRemember(r *accountModelTestRuntime) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !s.ownerLost {
		if err := s.repo.CheckOwner(ctx); err != nil {
			s.loseOwnerLocked()
		}
	}
	r.job.Persisted = !s.ownerLost && s.repo.SaveResults(ctx, r.adminID, []AccountModelTestJob{r.job}) == nil
	s.rememberResult(r)
}

func (s *AccountModelTestService) rememberResult(r *accountModelTestRuntime) {
	for id, previous := range s.recent {
		if previous.adminID == r.adminID && previous.job.AccountID == r.job.AccountID && id != r.job.ID {
			if previous.job.CreatedAt.After(r.job.CreatedAt) {
				return
			}
			delete(s.recent, id)
		}
	}
	s.recent[r.job.ID] = r
	s.pruneRecent()
}

func (s *AccountModelTestService) pruneRecent() {
	items := make([]*accountModelTestRuntime, 0, len(s.recent))
	total := 0
	for _, r := range s.recent {
		items = append(items, r)
		total += len(r.job.Prompt) + len(r.job.ResponseText) + len(r.job.ErrorMessage)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].job.CreatedAt.After(items[j].job.CreatedAt) })
	counts := map[int64]int{}
	for i, r := range items {
		counts[r.adminID]++
		if i >= accountModelTestMaxRecent || counts[r.adminID] > AccountModelTestMaxResults {
			delete(s.recent, r.job.ID)
			total -= len(r.job.Prompt) + len(r.job.ResponseText) + len(r.job.ErrorMessage)
		}
	}
	for i := len(items) - 1; i >= 0 && total > accountModelTestRecentBytes; i-- {
		r := items[i]
		if _, ok := s.recent[r.job.ID]; ok {
			delete(s.recent, r.job.ID)
			total -= len(r.job.Prompt) + len(r.job.ResponseText) + len(r.job.ErrorMessage)
		}
	}
}

func (s *AccountModelTestService) Cancel(adminID int64, id string) (*AccountModelTestJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.active[id]
	if r == nil {
		r = s.recent[id]
	}
	if r == nil || r.adminID != adminID || r.hidden {
		return nil, ErrModelTestNotFound
	}
	if !terminalAccountModelTestStatus(r.job.Status) {
		r.cancel()
		now := time.Now().UTC()
		r.job.Status = "cancelled"
		r.job.FinishedAt = &now
		r.job.ErrorMessage = "测试已取消"
		if r.job.StartedAt != nil {
			r.job.LatencyMs = now.Sub(*r.job.StartedAt).Milliseconds()
		}
		s.persistAndRemember(r)
	}
	job := r.job
	return &job, nil
}

func (s *AccountModelTestService) ListResults(ctx context.Context, adminID int64) (*AccountModelTestResults, error) {
	if err := s.Start(); err != nil {
		return nil, ErrModelTestUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listResultsLocked(ctx, adminID)
}

func (s *AccountModelTestService) listResultsLocked(ctx context.Context, adminID int64) (*AccountModelTestResults, error) {
	data, err := s.repo.ListResults(ctx, adminID)
	if data == nil {
		data = &AccountModelTestResults{Results: []AccountModelTestJob{}}
	}
	if cleared, ok := s.cleared[adminID]; ok && (data.ClearedAt == nil || cleared.After(*data.ClearedAt)) {
		data.ClearedAt = &cleared
	}
	byAccount := make(map[int64]AccountModelTestJob)
	for _, job := range data.Results {
		job.Persisted = true
		byAccount[job.AccountID] = job
	}
	fallback := false
	for _, r := range s.recent {
		if r.adminID != adminID || r.hidden || (data.ClearedAt != nil && !r.job.CreatedAt.After(*data.ClearedAt)) {
			continue
		}
		fallback = true
		previous, ok := byAccount[r.job.AccountID]
		if ok && previous.ID == r.job.ID && previous.Status == r.job.Status && previous.ResponseText == r.job.ResponseText && previous.ErrorMessage == r.job.ErrorMessage {
			r.job.Persisted = true
		}
		if !ok || r.job.CreatedAt.After(previous.CreatedAt) || (r.job.ID == previous.ID && !r.job.Persisted) {
			byAccount[r.job.AccountID] = r.job
		}
	}
	if err != nil && !fallback {
		return nil, ErrModelTestUnavailable
	}
	data.Results = make([]AccountModelTestJob, 0, len(byAccount))
	for _, job := range byAccount {
		data.Results = append(data.Results, job)
	}
	sortModelTestJobs(data.Results)
	if len(data.Results) > AccountModelTestMaxResults {
		data.Results = data.Results[:AccountModelTestMaxResults]
	}
	return data, nil
}

func sortModelTestJobs(jobs []AccountModelTestJob) {
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID > jobs[j].ID
		}
		return jobs[i].CreatedAt.After(jobs[j].CreatedAt)
	})
}

func (s *AccountModelTestService) ListJobs(ctx context.Context, adminID int64) ([]AccountModelTestJob, error) {
	if err := s.Start(); err != nil {
		return nil, ErrModelTestUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.listResultsLocked(ctx, adminID)
	jobs := []AccountModelTestJob{}
	if data != nil {
		jobs = append(jobs, data.Results...)
	}
	seen := map[string]bool{}
	for _, job := range jobs {
		seen[job.ID] = true
	}
	for _, r := range s.active {
		if r.adminID == adminID && !r.hidden && !seen[r.job.ID] {
			jobs = append(jobs, r.job)
		}
	}
	if err != nil && len(jobs) == 0 {
		return nil, err
	}
	sortModelTestJobs(jobs)
	return jobs, nil
}

func (s *AccountModelTestService) Import(ctx context.Context, adminID int64, jobs []AccountModelTestJob) (*AccountModelTestResults, error) {
	if len(jobs) > AccountModelTestMaxResults {
		return nil, errors.New("一次最多导入 500 条结果")
	}
	if err := s.Start(); err != nil {
		return nil, ErrModelTestUnavailable
	}
	for i := range jobs {
		job := &jobs[i]
		in := AccountModelTestInput{AccountID: job.AccountID, ModelID: job.ModelID, Prompt: job.Prompt, PromptKey: job.PromptKey, ReasoningEffort: job.ReasoningEffort}
		if err := validateAccountModelTestInput(&in); err != nil {
			return nil, fmt.Errorf("第 %d 条结果：%w", i+1, err)
		}
		parsedID, err := uuid.Parse(job.ID)
		if err != nil {
			return nil, errors.New("结果 ID 必须是 UUID")
		}
		job.ID = parsedID.String()
		if !terminalAccountModelTestStatus(job.Status) || job.CreatedAt.IsZero() || job.CreatedAt.After(time.Now()) || job.FinishedAt == nil || job.FinishedAt.Before(job.CreatedAt) || job.FinishedAt.After(time.Now()) || (job.StartedAt != nil && (job.StartedAt.Before(job.CreatedAt) || job.StartedAt.After(*job.FinishedAt))) || job.LatencyMs < 0 || len(job.ResponseText) > AccountModelTestMaxResponse || len(job.ErrorMessage) > 8192 {
			return nil, errors.New("导入结果状态、时间或内容长度无效")
		}
		account, err := s.accounts.GetByID(ctx, job.AccountID)
		if err != nil || account == nil {
			return nil, errors.New("导入结果包含不存在的账号")
		}
		job.AccountName = account.Name
		job.Platform = account.Platform
		job.Persisted = true
		job.ModelID = in.ModelID
		job.Prompt = in.Prompt
		job.ReasoningEffort = in.ReasoningEffort
		if job.Status == "success" && (strings.TrimSpace(job.ResponseText) == "" || (job.PromptKey == "pelican" && !validModelTestSVG(job.ResponseText))) {
			return nil, errors.New("成功结果缺少有效回答")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, ErrModelTestUnavailable
	}
	if err := s.repo.CheckOwner(ctx); err != nil {
		s.loseOwnerLocked()
		return nil, ErrModelTestUnavailable
	}
	for _, job := range jobs {
		if active := s.active[job.ID]; active != nil {
			return nil, errors.New("不能导入正在执行的任务 ID")
		}
	}
	if err := s.repo.SaveResults(ctx, adminID, jobs); err != nil {
		return nil, ErrModelTestUnavailable
	}
	return s.listResultsLocked(ctx, adminID)
}

func (s *AccountModelTestService) Clear(ctx context.Context, adminID int64) (*time.Time, error) {
	if err := s.Start(); err != nil {
		return nil, ErrModelTestUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, ErrModelTestUnavailable
	}
	if err := s.repo.CheckOwner(ctx); err != nil {
		s.loseOwnerLocked()
		return nil, ErrModelTestUnavailable
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.repo.Clear(ctx, adminID, now); err != nil {
		return nil, ErrModelTestUnavailable
	}
	s.cleared[adminID] = now
	for _, r := range s.active {
		if r.adminID == adminID {
			r.hidden = true
			r.cancel()
		}
	}
	for id, r := range s.recent {
		if r.adminID == adminID {
			delete(s.recent, id)
		}
	}
	return &now, nil
}

func (s *AccountModelTestService) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		ctx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		s.mu.Lock()
		s.stopped = true
		byAdmin := map[int64][]AccountModelTestJob{}
		for _, r := range s.active {
			// 先取消整个队列，再进行任何持久化，故障数据库不会延迟其他上游请求取消。
			r.cancel()
			if terminalAccountModelTestStatus(r.job.Status) {
				continue
			}
			now := time.Now().UTC()
			r.job.Status = "interrupted"
			r.job.FinishedAt = &now
			r.job.ErrorMessage = "服务退出，测试中断，请手动重试"
			r.job.Persisted = false
			if !r.hidden {
				s.rememberResult(r)
				byAdmin[r.adminID] = append(byAdmin[r.adminID], r.job)
			}
		}
		canPersist := !s.ownerLost
		s.mu.Unlock()
		if canPersist && s.repo.CheckOwner(ctx) == nil {
			for adminID, jobs := range byAdmin {
				if ctx.Err() != nil {
					break
				}
				if s.repo.SaveResults(ctx, adminID, jobs) == nil {
					s.mu.Lock()
					for _, job := range jobs {
						if r := s.recent[job.ID]; r != nil && r.job.ResponseText == job.ResponseText {
							r.job.Persisted = true
						}
					}
					s.mu.Unlock()
				}
			}
		}
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
		}
		_ = s.repo.Close()
	})
}

// monitorOwner 在独占连接丢失时关闭执行器，避免失锁后继续接受新付费请求。
func (s *AccountModelTestService) monitorOwner() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.repo.CheckOwner(ctx)
		cancel()
		if err != nil {
			s.mu.Lock()
			s.loseOwnerLocked()
			s.mu.Unlock()
			return
		}
	}
}

func (s *AccountModelTestService) loseOwnerLocked() {
	s.ownerLost = true
	s.stopped = true
	for _, r := range s.active {
		if terminalAccountModelTestStatus(r.job.Status) {
			continue
		}
		r.cancel()
		now := time.Now().UTC()
		r.job.Status = "interrupted"
		r.job.FinishedAt = &now
		r.job.ErrorMessage = "执行器失去数据库连接，测试已取消，请稍后手动重试"
		if !r.hidden {
			s.persistAndRemember(r)
		}
	}
}
