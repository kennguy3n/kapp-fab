-- Phase RBAC — role hierarchy via parent_role.
--
-- Adding a baseline permission to `tenant.member` previously required
-- editing every role's permissions JSONB blob individually — a manual
-- and error-prone process for any tenant that has customised their
-- roles. With a parent pointer the evaluator can walk the chain and
-- union permissions from each ancestor, so adding a permission to
-- `tenant.member` automatically lifts the floor for every role that
-- inherits from it.
--
-- The cycle prevention is enforced application-side (the role
-- management API rejects updates that would create a cycle) and the
-- evaluator's recursive CTE has a depth bound (5) as a backstop so a
-- broken state cannot stall a request indefinitely.

ALTER TABLE roles ADD COLUMN IF NOT EXISTS parent_role TEXT;

-- Default hierarchy:
--   owner -> tenant.admin -> tenant.member
--   <module>.* roles inherit tenant.member so cross-cutting baseline
--   permissions (read profile, list KTypes, etc.) propagate without
--   touching each module role.
UPDATE roles SET parent_role = 'tenant.member'
 WHERE parent_role IS NULL
   AND name IN (
       'finance.admin',
       'hr.admin',
       'lms.admin',
       'crm.rep',
       'crm.manager',
       'inventory.admin',
       'helpdesk.agent',
       'helpdesk.manager',
       'sales.rep',
       'procurement.rep',
       'reporting.viewer'
   );

UPDATE roles SET parent_role = 'tenant.member'
 WHERE parent_role IS NULL
   AND name = 'tenant.admin';

UPDATE roles SET parent_role = 'tenant.admin'
 WHERE parent_role IS NULL
   AND name = 'owner';
