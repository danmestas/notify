-- 0015_translation_settings: env-level translation config columns.
-- translation_default_locale holds the BCP-47 locale code for the reference
-- key set (e.g. "en"). translation_target_locales is a JSON array of locale
-- codes the team maintains (e.g. '["es","fr"]').
ALTER TABLE environments ADD COLUMN translation_default_locale TEXT;
ALTER TABLE environments ADD COLUMN translation_target_locales TEXT;
