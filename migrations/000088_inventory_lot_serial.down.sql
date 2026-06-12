-- Rollback for 000088_inventory_lot_serial.sql. Drops in reverse
-- dependency order: the junction (which FKs both moves and serials)
-- first, then the serial master, then the item-level flags.

DROP TABLE IF EXISTS inventory_move_serials;
DROP TABLE IF EXISTS inventory_serials;

ALTER TABLE inventory_items DROP COLUMN IF EXISTS serial_tracked;
ALTER TABLE inventory_items DROP COLUMN IF EXISTS lot_tracked;
