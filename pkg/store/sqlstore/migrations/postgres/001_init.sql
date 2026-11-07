CREATE TABLE users (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE scale_readings (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  measured_at TIMESTAMPTZ NOT NULL,
  weight_kg DOUBLE PRECISION, fat_ratio_pct DOUBLE PRECISION, fat_mass_kg DOUBLE PRECISION, fat_free_mass_kg DOUBLE PRECISION,
  muscle_mass_kg DOUBLE PRECISION, hydration_kg DOUBLE PRECISION, bone_mass_kg DOUBLE PRECISION, visceral_fat_index DOUBLE PRECISION,
  source TEXT NOT NULL DEFAULT 'manual',
  external_id TEXT,
  notes TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE (user_id, source, external_id)
);
CREATE INDEX idx_scale_user_time ON scale_readings(user_id, measured_at);
CREATE TABLE dexa_scans (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  measured_at DATE NOT NULL,
  weight_kg DOUBLE PRECISION, fat_ratio_pct DOUBLE PRECISION, fat_mass_kg DOUBLE PRECISION, fat_free_mass_kg DOUBLE PRECISION,
  lean_mass_kg DOUBLE PRECISION, bone_mass_kg DOUBLE PRECISION, visceral_fat_mass_kg DOUBLE PRECISION,
  bmd_g_cm2 DOUBLE PRECISION, t_score DOUBLE PRECISION, z_score DOUBLE PRECISION, android_fat_pct DOUBLE PRECISION, gynoid_fat_pct DOUBLE PRECISION,
  notes TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_dexa_user_time ON dexa_scans(user_id, measured_at);
CREATE TABLE user_integrations (
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '{}',
  updated_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (user_id, provider)
);
