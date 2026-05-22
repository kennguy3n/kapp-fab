-- Phase 2.2: PostgreSQL LISTEN/NOTIFY trigger on the events outbox.
-- The worker subscribes to "kapp_events" with a 2-second fallback timeout.
-- On INSERT, pg_notify fires and the worker wakes immediately, reducing
-- delivery latency from the old 2-second ticker cadence to sub-50ms.
-- The payload carries the tenant_id so a future multi-worker deployment can
-- route notifications to the worker responsible for a given tenant shard.

CREATE OR REPLACE FUNCTION notify_new_event() RETURNS trigger AS $$
BEGIN
  PERFORM pg_notify('kapp_events', NEW.tenant_id::text);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER events_after_insert
  AFTER INSERT ON events
  FOR EACH ROW EXECUTE FUNCTION notify_new_event();
