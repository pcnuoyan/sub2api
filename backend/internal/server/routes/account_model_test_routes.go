package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

func registerAccountModelTestRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	if h.Admin.AccountModelTest == nil {
		return
	}
	test := h.Admin.AccountModelTest
	admin.GET("/account-model-test-jobs", test.Jobs)
	admin.POST("/account-model-test-jobs", test.Create)
	admin.DELETE("/account-model-test-jobs/:id", test.Cancel)
	admin.GET("/account-model-test-results", test.Results)
	admin.POST("/account-model-test-results", test.Import)
	admin.DELETE("/account-model-test-results", test.Clear)
}
