package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type accountModelTestRepository struct {
	db        *sql.DB
	owner     *sql.Conn
	closeOnce sync.Once
}

const accountModelTestOwnerLock int64 = 900001260001

func NewAccountModelTestRepository(db *sql.DB) service.AccountModelTestRepository {
	return &accountModelTestRepository{db: db}
}

// lockModelTestState 串行化同一管理员的导入、完成和清空，防止旧结果重新出现。
func lockModelTestState(ctx context.Context, tx *sql.Tx, adminID int64) (*time.Time, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_account_model_test_state(admin_id) VALUES($1) ON CONFLICT DO NOTHING`, adminID); err != nil {
		return nil, err
	}
	var cleared sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT cleared_at FROM admin_account_model_test_state WHERE admin_id=$1 FOR UPDATE`, adminID).Scan(&cleared); err != nil {
		return nil, err
	}
	if cleared.Valid {
		return &cleared.Time, nil
	}
	return nil, nil
}

func (r *accountModelTestRepository) PutPending(ctx context.Context, adminID int64, job service.AccountModelTestJob) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cleared, err := lockModelTestState(ctx, tx, adminID)
	if err != nil {
		return err
	}
	if cleared != nil && !job.CreatedAt.After(*cleared) {
		return tx.Commit()
	}
	job.Persisted = true
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_account_model_test_jobs(id,admin_id,account_id,requested_at,job_data)
		VALUES($1,$2,$3,$4,$5::jsonb) ON CONFLICT(id) DO UPDATE SET job_data=EXCLUDED.job_data
		WHERE admin_account_model_test_jobs.admin_id=EXCLUDED.admin_id`, job.ID, adminID, job.AccountID, job.CreatedAt, string(data))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *accountModelTestRepository) SaveResults(ctx context.Context, adminID int64, jobs []service.AccountModelTestJob) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cleared, err := lockModelTestState(ctx, tx, adminID)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		// 请求时间较旧的任务即使晚完成，也不能覆盖较新的账号结果。
		if cleared == nil || job.CreatedAt.After(*cleared) {
			job.Persisted = true
			data, err := json.Marshal(job)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO admin_account_model_test_results(admin_id,account_id,requested_at,job_data)
				VALUES($1,$2,$3,$4::jsonb) ON CONFLICT(admin_id,account_id) DO UPDATE
				SET requested_at=EXCLUDED.requested_at,job_data=EXCLUDED.job_data
				WHERE admin_account_model_test_results.requested_at < EXCLUDED.requested_at
				OR (admin_account_model_test_results.requested_at = EXCLUDED.requested_at
				AND admin_account_model_test_results.job_data->>'id'=EXCLUDED.job_data->>'id')`, adminID, job.AccountID, job.CreatedAt, string(data))
			if err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM admin_account_model_test_jobs WHERE admin_id=$1 AND id=$2`, adminID, job.ID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM admin_account_model_test_results WHERE admin_id=$1 AND account_id IN
		(SELECT account_id FROM admin_account_model_test_results WHERE admin_id=$1 ORDER BY requested_at DESC,account_id DESC OFFSET 500)`, adminID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *accountModelTestRepository) ListResults(ctx context.Context, adminID int64) (*service.AccountModelTestResults, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// 同一快照读取水位与结果，避免读取期间清空导致旧结果短暂回流。
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result := &service.AccountModelTestResults{Results: []service.AccountModelTestJob{}}
	var cleared sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT cleared_at FROM admin_account_model_test_state WHERE admin_id=$1`, adminID).Scan(&cleared)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if cleared.Valid {
		result.ClearedAt = &cleared.Time
	}
	rows, err := tx.QueryContext(ctx, `SELECT job_data FROM admin_account_model_test_results WHERE admin_id=$1 ORDER BY requested_at DESC,account_id DESC LIMIT 500`, adminID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var job service.AccountModelTestJob
		if err := json.Unmarshal(data, &job); err != nil {
			_ = rows.Close()
			return nil, err
		}
		job.Persisted = true
		result.Results = append(result.Results, job)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *accountModelTestRepository) Clear(ctx context.Context, adminID int64, at time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockModelTestState(ctx, tx, adminID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE admin_account_model_test_state SET cleared_at=GREATEST(COALESCE(cleared_at,$2),$2) WHERE admin_id=$1`, adminID, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_account_model_test_results WHERE admin_id=$1`, adminID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_account_model_test_jobs WHERE admin_id=$1`, adminID); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverInterrupted 仅在单实例服务启动时调用；请求只标记中断，不自动消耗额度重试。
func (r *accountModelTestRepository) RecoverInterrupted(ctx context.Context) error {
	// 独占数据库会话锁确保只有一个执行器可恢复并执行任务，第二个实例仅禁用此模块。
	owner, err := r.db.Conn(ctx)
	if err != nil {
		return err
	}
	var acquired bool
	if err := owner.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, accountModelTestOwnerLock).Scan(&acquired); err != nil {
		_ = owner.Close()
		return err
	}
	if !acquired {
		_ = owner.Close()
		return errors.New("已有其他实例运行账号模型测试执行器")
	}
	r.owner = owner
	success := false
	defer func() {
		if !success {
			_ = r.Close()
		}
	}()
	rows, err := r.db.QueryContext(ctx, `SELECT admin_id,job_data FROM admin_account_model_test_jobs ORDER BY requested_at ASC`)
	if err != nil {
		return err
	}
	byAdmin := map[int64][]service.AccountModelTestJob{}
	now := time.Now().UTC()
	for rows.Next() {
		var adminID int64
		var data []byte
		if err := rows.Scan(&adminID, &data); err != nil {
			_ = rows.Close()
			return err
		}
		var job service.AccountModelTestJob
		if err := json.Unmarshal(data, &job); err != nil {
			_ = rows.Close()
			return err
		}
		job.Status = "interrupted"
		job.ErrorMessage = "服务重启，测试中断，请手动重试"
		job.FinishedAt = &now
		byAdmin[adminID] = append(byAdmin[adminID], job)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for adminID, jobs := range byAdmin {
		if err := r.SaveResults(ctx, adminID, jobs); err != nil {
			return err
		}
	}
	success = true
	return nil
}

// Close 显式释放会话锁后再归还连接，避免锁遗留在连接池。
func (r *accountModelTestRepository) Close() error {
	var closeErr error
	r.closeOnce.Do(func() {
		if r.owner == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, closeErr = r.owner.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, accountModelTestOwnerLock)
		if err := r.owner.Close(); closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}

func (r *accountModelTestRepository) CheckOwner(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if r.owner == nil {
		return errors.New("账号测试执行器没有独占连接")
	}
	var held bool
	err := r.owner.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory'
		AND pid=pg_backend_pid() AND classid=$1 AND objid=$2 AND objsubid=1 AND granted)`, accountModelTestOwnerLock>>32, accountModelTestOwnerLock&0xffffffff).Scan(&held)
	if err != nil {
		return err
	}
	if !held {
		return errors.New("账号测试执行器已失去独占锁")
	}
	return nil
}
