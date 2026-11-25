package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type modelTestHandlerRepo struct {
	mu     sync.Mutex
	owners []int64
}

func (r *modelTestHandlerRepo) RecoverInterrupted(context.Context) error { return nil }
func (r *modelTestHandlerRepo) CheckOwner(context.Context) error         { return nil }
func (r *modelTestHandlerRepo) Close() error                             { return nil }
func (r *modelTestHandlerRepo) PutPending(_ context.Context, id int64, _ service.AccountModelTestJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners = append(r.owners, id)
	return nil
}
func (r *modelTestHandlerRepo) SaveResults(_ context.Context, id int64, _ []service.AccountModelTestJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners = append(r.owners, id)
	return nil
}
func (r *modelTestHandlerRepo) ListResults(_ context.Context, id int64) (*service.AccountModelTestResults, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners = append(r.owners, id)
	return &service.AccountModelTestResults{Results: []service.AccountModelTestJob{}}, nil
}
func (r *modelTestHandlerRepo) Clear(context.Context, int64, time.Time) error { return nil }

type modelTestHandlerAccounts struct{}

func (modelTestHandlerAccounts) GetByID(context.Context, int64) (*service.Account, error) {
	return &service.Account{ID: 1, Name: "测试账户", Platform: service.PlatformOpenAI}, nil
}

type modelTestHandlerExecutor struct{}

func (modelTestHandlerExecutor) RunModelTestBackground(context.Context, int64, string, string, string) (*service.ScheduledTestResult, error) {
	return &service.ScheduledTestResult{Status: "success", ResponseText: "测试回答"}, nil
}

func TestAccountModelTestHandlerRejectsMissingRoleAndRestrictedKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, role string
		userID     int64
		restricted bool
	}{{name: "missing"}, {name: "user", role: "user", userID: 1}, {name: "restricted", role: "admin", userID: 1, restricted: true}} {
		t.Run(test.name, func(t *testing.T) {
			h := NewAccountModelTestHandler(nil)
			for _, handle := range []gin.HandlerFunc{h.Create, h.Jobs, h.Cancel, h.Results, h.Import, h.Clear} {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"admin_id":1}`))
				if test.userID > 0 {
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: test.userID})
					c.Set(string(middleware.ContextKeyUserRole), test.role)
				}
				if test.restricted {
					c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{CreatorID: 1}))
				}
				handle(c)
				require.Equal(t, http.StatusForbidden, w.Code)
			}
		})
	}
}

func TestAccountModelTestHandlerUsesAuthenticatedAdministrator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &modelTestHandlerRepo{}
	svc := service.NewAccountModelTestService(repo, modelTestHandlerAccounts{}, modelTestHandlerExecutor{})
	defer svc.Stop()
	h := NewAccountModelTestHandler(svc)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"admin_id":999,"account_id":1,"model_id":"text-model","prompt":"题目","prompt_key":"custom","reasoning_effort":""}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 17})
	c.Set(string(middleware.ContextKeyUserRole), "admin")
	h.Create(c)
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Code int                         `json:"code"`
		Data service.AccountModelTestJob `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Zero(t, body.Code)
	require.NotEmpty(t, body.Data.ID)
	require.Equal(t, int64(1), body.Data.AccountID)
	svc.Stop()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.NotEmpty(t, repo.owners)
	for _, id := range repo.owners {
		require.Equal(t, int64(17), id)
	}
}

func TestAccountModelTestHandlerRejectsOversizedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAccountModelTestHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"prompt":"`+strings.Repeat("x", 33<<10)+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
	c.Set(string(middleware.ContextKeyUserRole), "admin")
	h.Create(c)
	require.Equal(t, http.StatusBadRequest, w.Code)
}
