-- 0008_translations: per-locale message strings keyed by (environment, locale,
-- key). Delivery handlers render an in_app/email template through a {{t}}
-- helper backed by Map(scope, locale), so an authored {{t "greeting"}}
-- resolves to the subscriber's localized string.

CREATE TABLE translations (
    id              TEXT PRIMARY KEY,
    org_id          TEXT,
    environment_id  TEXT NOT NULL,
    locale          TEXT NOT NULL,
    key             TEXT NOT NULL,
    value           TEXT,
    UNIQUE (environment_id, locale, key)
);

CREATE INDEX translations_env_locale ON translations (environment_id, locale);
