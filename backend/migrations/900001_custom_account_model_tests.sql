-- 自有功能使用独立高位编号，避免与上游正常递增迁移重名。
-- 本模块仅用于管理员手动测试，不创建定时任务或公开结果入口。
CREATE TABLE IF NOT EXISTS admin_account_model_test_state (
    admin_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    cleared_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS admin_account_model_test_jobs (
    id UUID PRIMARY KEY,
    admin_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    requested_at TIMESTAMPTZ NOT NULL,
    job_data JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_admin_account_model_test_jobs_admin
    ON admin_account_model_test_jobs(admin_id, requested_at DESC);

CREATE TABLE IF NOT EXISTS admin_account_model_test_results (
    admin_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    requested_at TIMESTAMPTZ NOT NULL,
    job_data JSONB NOT NULL,
    PRIMARY KEY (admin_id, account_id)
);
CREATE INDEX IF NOT EXISTS idx_admin_account_model_test_results_recent
    ON admin_account_model_test_results(admin_id, requested_at DESC);
