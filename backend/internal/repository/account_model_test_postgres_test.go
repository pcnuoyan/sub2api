//go:build modeltestdb

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// TestAccountModelTestPostgres 用隔离的本地 schema 验证真实 PostgreSQL 事务与恢复语义。
// 只接受回环地址，不能通过本用例连接或清理生产数据库。
func TestAccountModelTestPostgres(t *testing.T) {
	dsn := os.Getenv("ACCOUNT_MODEL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("需要独立本地 PostgreSQL 测试地址")
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"127.0.0.1", "localhost", "::1"}, u.Hostname())
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	rootDB, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer rootDB.Close()
	schema := "modeltest_" + fmt.Sprint(time.Now().UnixNano())
	_, err = rootDB.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	defer func() { _, _ = rootDB.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(ctx, "CREATE TABLE users(id BIGINT PRIMARY KEY); CREATE TABLE accounts(id BIGINT PRIMARY KEY); INSERT INTO users VALUES(1),(2); INSERT INTO accounts SELECT generate_series(1,510)")
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("900001_custom_account_model_tests.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err, "迁移须可重复执行")
	repo := NewAccountModelTestRepository(db)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	job := func(account int64, offset time.Duration) service.AccountModelTestJob {
		at := base.Add(offset)
		finished := at.Add(time.Second)
		return service.AccountModelTestJob{ID: uuid.NewString(), AccountID: account, AccountName: "本地验证", Platform: "openai", ModelID: "6a", Prompt: "验证", PromptKey: "custom", Status: "success", ResponseText: "完成", CreatedAt: at, StartedAt: &at, FinishedAt: &finished, Persisted: true}
	}
	newer, older := job(1, time.Minute), job(1, 0)
	require.NoError(t, repo.SaveResults(ctx, 1, []service.AccountModelTestJob{newer, older}))
	require.NoError(t, repo.SaveResults(ctx, 2, []service.AccountModelTestJob{older}))
	first, err := repo.ListResults(ctx, 1)
	require.NoError(t, err)
	require.Len(t, first.Results, 1)
	require.Equal(t, newer.ID, first.Results[0].ID, "旧任务不能覆盖新任务")
	second, err := repo.ListResults(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, older.ID, second.Results[0].ID, "同一账号按管理员分别保存")

	cutoff := base.Add(2 * time.Minute)
	var group sync.WaitGroup
	errorsCh := make(chan error, 12)
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			if index%2 == 0 {
				errorsCh <- repo.Clear(ctx, 1, cutoff)
			} else {
				errorsCh <- repo.SaveResults(ctx, 1, []service.AccountModelTestJob{newer})
			}
		}(i)
	}
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		require.NoError(t, err)
	}
	first, err = repo.ListResults(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, first.Results, "并发清空后旧结果不得复活")
	require.True(t, first.ClearedAt.Equal(cutoff))
	second, err = repo.ListResults(ctx, 2)
	require.NoError(t, err)
	require.Len(t, second.Results, 1, "清空不能影响另一管理员")

	batch := make([]service.AccountModelTestJob, 510)
	for i := range batch {
		batch[i] = job(int64(i+1), 3*time.Minute+time.Duration(i)*time.Millisecond)
	}
	require.NoError(t, repo.SaveResults(ctx, 1, batch))
	first, err = repo.ListResults(ctx, 1)
	require.NoError(t, err)
	require.Len(t, first.Results, 500)
	require.Equal(t, int64(510), first.Results[0].AccountID)
	require.Equal(t, int64(11), first.Results[499].AccountID)

	pending := job(510, 4*time.Minute)
	pending.Status = "running"
	pending.FinishedAt = nil
	require.NoError(t, repo.PutPending(ctx, 1, pending))
	require.NoError(t, repo.RecoverInterrupted(ctx))
	owner := repo.(interface {
		CheckOwner(context.Context) error
		Close() error
	})
	defer owner.Close()
	require.NoError(t, owner.CheckOwner(ctx))
	first, err = repo.ListResults(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, "interrupted", first.Results[0].Status)
	require.Equal(t, pending.ID, first.Results[0].ID)
	var remaining int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM admin_account_model_test_jobs").Scan(&remaining))
	require.Zero(t, remaining)
	competitor := NewAccountModelTestRepository(db)
	require.Error(t, competitor.RecoverInterrupted(ctx), "第二实例不得恢复正在运行实例的任务")
}
