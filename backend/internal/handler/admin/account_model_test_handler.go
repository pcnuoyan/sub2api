package admin

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type AccountModelTestHandler struct {
	service *service.AccountModelTestService
}

func NewAccountModelTestHandler(svc *service.AccountModelTestService) *AccountModelTestHandler {
	return &AccountModelTestHandler{service: svc}
}

// modelTestAdminID 只使用认证身份；受限管理密钥不能读取创建者的全部测试记录。
func modelTestAdminID(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	role, roleOK := middleware.GetUserRoleFromContext(c)
	if !ok || subject.UserID <= 0 || !roleOK || role != "admin" {
		response.Error(c, http.StatusForbidden, "需要管理员身份")
		return 0, false
	}
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, http.StatusForbidden, "受限管理密钥不能使用账号模型测试")
		return 0, false
	}
	return subject.UserID, true
}

func respondModelTestError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrModelTestNotFound):
		response.NotFound(c, err.Error())
	case errors.Is(err, service.ErrModelTestBusy):
		response.Error(c, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrModelTestUnavailable):
		response.Error(c, http.StatusServiceUnavailable, err.Error())
	default:
		response.BadRequest(c, err.Error())
	}
}

func (h *AccountModelTestHandler) Create(c *gin.Context) {
	adminID, ok := modelTestAdminID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var in service.AccountModelTestInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, "测试请求格式或长度无效")
		return
	}
	job, err := h.service.Create(c.Request.Context(), adminID, in)
	if err != nil {
		respondModelTestError(c, err)
		return
	}
	response.Success(c, job)
}

func (h *AccountModelTestHandler) Jobs(c *gin.Context) {
	adminID, ok := modelTestAdminID(c)
	if !ok {
		return
	}
	jobs, err := h.service.ListJobs(c.Request.Context(), adminID)
	if err != nil {
		respondModelTestError(c, err)
		return
	}
	response.Success(c, gin.H{"jobs": jobs})
}

func (h *AccountModelTestHandler) Cancel(c *gin.Context) {
	adminID, ok := modelTestAdminID(c)
	if !ok {
		return
	}
	job, err := h.service.Cancel(adminID, c.Param("id"))
	if err != nil {
		respondModelTestError(c, err)
		return
	}
	response.Success(c, job)
}

func (h *AccountModelTestHandler) Results(c *gin.Context) {
	adminID, ok := modelTestAdminID(c)
	if !ok {
		return
	}
	data, err := h.service.ListResults(c.Request.Context(), adminID)
	if err != nil {
		respondModelTestError(c, err)
		return
	}
	response.Success(c, data)
}

func (h *AccountModelTestHandler) Import(c *gin.Context) {
	adminID, ok := modelTestAdminID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<20)
	var in struct {
		Results []service.AccountModelTestJob `json:"results"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, "导入格式无效或文件超过 16 MiB")
		return
	}
	data, err := h.service.Import(c.Request.Context(), adminID, in.Results)
	if err != nil {
		respondModelTestError(c, err)
		return
	}
	response.Success(c, data)
}

func (h *AccountModelTestHandler) Clear(c *gin.Context) {
	adminID, ok := modelTestAdminID(c)
	if !ok {
		return
	}
	cleared, err := h.service.Clear(c.Request.Context(), adminID)
	if err != nil {
		respondModelTestError(c, err)
		return
	}
	response.Success(c, gin.H{"cleared_at": cleared})
}
