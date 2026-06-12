-- Rollback for 000089_inventory_lot_serial_projection.sql.

ALTER TABLE inventory_serials DROP CONSTRAINT IF EXISTS inventory_serials_status_valid;
ALTER TABLE inventory_batches DROP CONSTRAINT IF EXISTS inventory_batches_qty_on_hand_nonneg;
DROP VIEW IF EXISTS stock_levels_by_batch;
