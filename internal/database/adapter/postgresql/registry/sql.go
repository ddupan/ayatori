/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package registry

const claimStatement = `
INSERT INTO postgresql_tenant_operator.tenant_ownership (
    instance_uid,
    tenant_uid,
    tenant_namespace,
    tenant_name,
    database_name,
    role_name,
    credential_path
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT DO NOTHING
RETURNING
    instance_uid,
    tenant_uid,
    tenant_namespace,
    tenant_name,
    database_name,
    role_name,
    credential_path,
    managed,
    created_at,
    updated_at,
    retained_at`

const getByTenantUIDStatement = `
SELECT
    instance_uid,
    tenant_uid,
    tenant_namespace,
    tenant_name,
    database_name,
    role_name,
    credential_path,
    managed,
    created_at,
    updated_at,
    retained_at
FROM postgresql_tenant_operator.tenant_ownership
WHERE instance_uid = $1 AND tenant_uid = $2`

const getByTenantUIDForUpdateStatement = getByTenantUIDStatement + ` FOR UPDATE`

const markRetainedStatement = `
UPDATE postgresql_tenant_operator.tenant_ownership
SET managed = false, retained_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE instance_uid = $1 AND tenant_uid = $2 AND managed = true`

const deleteStatement = `
DELETE FROM postgresql_tenant_operator.tenant_ownership
WHERE instance_uid = $1 AND tenant_uid = $2 AND managed = true`
