UPDATE workspaces
SET last_access_at = CURRENT_TIMESTAMP
WHERE deleted_at IS NULL
  AND desired_state = 'running';
