CREATE SCHEMA postgresql_tenant_operator;

CREATE TABLE postgresql_tenant_operator.tenant_ownership (
    instance_uid text NOT NULL,
    tenant_uid text NOT NULL,
    tenant_namespace text NOT NULL,
    tenant_name text NOT NULL,
    database_name text NOT NULL,
    role_name text NOT NULL,
    credential_path text NOT NULL,
    managed boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    retained_at timestamptz,
    PRIMARY KEY (instance_uid, tenant_uid),
    UNIQUE (instance_uid, tenant_namespace, tenant_name),
    UNIQUE (instance_uid, database_name),
    UNIQUE (instance_uid, role_name),
    UNIQUE (credential_path),
    CHECK (managed OR retained_at IS NOT NULL)
);

---- create above / drop below ----

DROP SCHEMA postgresql_tenant_operator CASCADE;
