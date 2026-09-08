CREATE TABLE IF NOT EXISTS logs
(
    -- Inserted by the exporter, in its INSERT order (OTel Logs data model).
    -- Timestamp is event time: unreliable, so not the query key.
    Timestamp DateTime64(9),
    TraceId String,
    SpanId String,
    TraceFlags UInt8,
    SeverityText LowCardinality(String),
    SeverityNumber UInt8,
    ServiceName LowCardinality(String),
    Body String,
    ResourceSchemaUrl LowCardinality(String),
    ResourceAttributes Map(String, String),
    ScopeSchemaUrl LowCardinality(String),
    ScopeName String,
    ScopeVersion LowCardinality(String),
    ScopeAttributes Map(String, String),
    LogAttributes Map(String, String),
    EventName String,

    -- Derived, never inserted -- the exporter's column list is fixed.
    -- Collector receipt time is the query/partition/order key, so a drain or a
    -- bad source clock cannot shift it. now64(9) floors it: 0 matches nothing.
    ObservedTimestamp DateTime64(9) MATERIALIZED
        if(LogAttributes['telemetry.observed_time_unix_nano'] != '',
           fromUnixTimestamp64Nano(toInt64OrZero(LogAttributes['telemetry.observed_time_unix_nano'])),
           now64(9)),
    ProjectId String MATERIALIZED ResourceAttributes['milo.project.id']
)
ENGINE = MergeTree
-- Monthly, not daily: N projects x days would explode the partition count.
-- No TTL here -- retention is the deployment repo's call, not a shared schema's.
PARTITION BY toYYYYMM(ObservedTimestamp)
ORDER BY (ProjectId, ObservedTimestamp, ServiceName);

-- {{QUERYAPI_USER}} comes from CLICKHOUSE_QUERYAPI_USER. Grants live in the
-- deployment's users.d (SQL cannot GRANT to users_xml), but policies attach.
-- Unset setting, no rows.

CREATE ROW POLICY IF NOT EXISTS queryapi_project_isolation
ON logs
FOR SELECT
USING ProjectId = getSetting('telemetry_project_id')
TO `{{QUERYAPI_USER}}`;
