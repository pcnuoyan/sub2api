package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/logger"

func ProvideAccountModelTestService(repo AccountModelTestRepository, accounts AccountRepository, executor *AccountTestService) *AccountModelTestService {
	svc := NewAccountModelTestService(repo, accounts, executor)
	if err := svc.Start(); err != nil {
		logger.LegacyPrintf("service.account_model_test", "账号模型测试初始化失败，功能暂不可用: %v", err)
	}
	return svc
}
