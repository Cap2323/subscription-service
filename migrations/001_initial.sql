CREATE TABLE public.routing_guard (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    shard_id TEXT NOT NULL DEFAULT '',
    fingerprint TEXT NOT NULL DEFAULT '',
    epoch BIGINT NOT NULL DEFAULT 0,
    frozen BOOLEAN NOT NULL DEFAULT TRUE,
    pending_fingerprint TEXT NOT NULL DEFAULT '',
    copy_complete BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO public.routing_guard(singleton) VALUES (TRUE);

-- Runtime can acquire a shared fence lock, but cannot edit the topology fence.
CREATE FUNCTION public.check_routing(expected_shard TEXT, expected_fingerprint TEXT)
RETURNS BOOLEAN LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE g public.routing_guard%ROWTYPE;
BEGIN
    SELECT * INTO STRICT g FROM public.routing_guard WHERE singleton FOR SHARE;
    RETURN NOT g.frozen AND g.shard_id = expected_shard AND g.fingerprint = expected_fingerprint;
END;
$$;
REVOKE ALL ON FUNCTION public.check_routing(TEXT,TEXT) FROM PUBLIC;

CREATE TABLE public.subscriptions (
    bucket_id INTEGER NOT NULL CHECK (bucket_id BETWEEN 0 AND 65535),
    user_id UUID NOT NULL CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    id UUID NOT NULL,
    service_name VARCHAR(255) NOT NULL CHECK (char_length(btrim(service_name)) BETWEEN 1 AND 255),
    price INTEGER NOT NULL CHECK (price BETWEEN 1 AND 2147483647),
    start_date DATE NOT NULL CHECK (start_date BETWEEN DATE '0001-01-01' AND DATE '9999-12-01' AND EXTRACT(DAY FROM start_date)=1),
    end_date DATE CHECK (end_date IS NULL OR (end_date BETWEEN start_date AND DATE '9999-12-01' AND EXTRACT(DAY FROM end_date)=1)),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (bucket_id,user_id,id)
);
CREATE INDEX subscriptions_page_idx ON public.subscriptions (bucket_id,user_id,created_at DESC,id DESC);
CREATE INDEX subscriptions_name_page_idx ON public.subscriptions (bucket_id,user_id,service_name,created_at DESC,id DESC);

CREATE TABLE public.user_usage (
    bucket_id INTEGER NOT NULL CHECK (bucket_id BETWEEN 0 AND 65535),
    user_id UUID NOT NULL CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    subscription_count INTEGER NOT NULL CHECK (subscription_count BETWEEN 0 AND 10000),
    PRIMARY KEY(bucket_id,user_id)
);
CREATE TABLE public.idempotency (
    bucket_id INTEGER NOT NULL CHECK (bucket_id BETWEEN 0 AND 65535),
    user_id UUID NOT NULL CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    key_hash BYTEA NOT NULL CHECK (octet_length(key_hash)=32),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash)=32),
    response JSONB,
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (clock_timestamp()+INTERVAL '24 hours'),
    PRIMARY KEY(bucket_id,user_id,key_hash)
);
CREATE INDEX idempotency_expiry_idx ON public.idempotency(expires_at);

ALTER TABLE public.subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.subscriptions FORCE ROW LEVEL SECURITY;
ALTER TABLE public.user_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.user_usage FORCE ROW LEVEL SECURITY;
ALTER TABLE public.idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.idempotency FORCE ROW LEVEL SECURITY;
CREATE POLICY subscriptions_owner ON public.subscriptions USING (user_id=NULLIF(current_setting('app.user_id',TRUE),'')::uuid) WITH CHECK (user_id=NULLIF(current_setting('app.user_id',TRUE),'')::uuid);
CREATE POLICY usage_owner ON public.user_usage USING (user_id=NULLIF(current_setting('app.user_id',TRUE),'')::uuid) WITH CHECK (user_id=NULLIF(current_setting('app.user_id',TRUE),'')::uuid);
CREATE POLICY idempotency_owner ON public.idempotency USING (user_id=NULLIF(current_setting('app.user_id',TRUE),'')::uuid) WITH CHECK (user_id=NULLIF(current_setting('app.user_id',TRUE),'')::uuid);

GRANT USAGE ON SCHEMA public TO subscriptions_app;
GRANT SELECT,INSERT,UPDATE,DELETE ON public.subscriptions, public.user_usage, public.idempotency TO subscriptions_app;
GRANT SELECT ON public.routing_guard TO subscriptions_app;
GRANT EXECUTE ON FUNCTION public.check_routing(TEXT,TEXT) TO subscriptions_app;
