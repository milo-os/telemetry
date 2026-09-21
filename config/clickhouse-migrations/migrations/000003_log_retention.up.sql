-- Opinionated default: internal (provider's own) data moves to cold storage
-- almost immediately and is deleted after 30 days. Everything else stays on
-- hot storage for its whole life and is deleted after 7 days.
ALTER TABLE logs MODIFY SETTING storage_policy = 'hot_cold';

ALTER TABLE logs MODIFY TTL
    toDateTime(ObservedTimestamp) + toIntervalDay(if(ProjectId = 'internal', 1, 36500)) TO VOLUME 'cold',
    toDateTime(ObservedTimestamp) + toIntervalDay(if(ProjectId = 'internal', 30, 7)) DELETE
