DROP INDEX IF EXISTS notifications_retry_idx;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_status_chk;
ALTER TABLE notifications
    DROP COLUMN IF EXISTS delivery_status,
    DROP COLUMN IF EXISTS delivery_note,
    DROP COLUMN IF EXISTS attempts,
    DROP COLUMN IF EXISTS delivered_channels,
    DROP COLUMN IF EXISTS last_delivery_at;
