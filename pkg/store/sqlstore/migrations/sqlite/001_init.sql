CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  created_at TIMESTAMP NOT NULL
);
CREATE TABLE scale_readings (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  measured_at TIMESTAMP NOT NULL,
  weight_kg REAL, fat_ratio_pct REAL, fat_mass_kg REAL, fat_free_mass_kg REAL,
  muscle_mass_kg REAL, hydration_kg REAL, bone_mass_kg REAL, visceral_fat_index REAL,
  source TEXT NOT NULL DEFAULT 'manual',
  external_id TEXT,
  notes TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL,
  UNIQUE (user_id, source, external_id)
);
CREATE INDEX idx_scale_user_time ON scale_readings(user_id, measured_at);
CREATE TABLE dexa_scans (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  measured_at DATE NOT NULL,
  weight_kg REAL, fat_ratio_pct REAL, fat_mass_kg REAL, fat_free_mass_kg REAL,
  lean_mass_kg REAL, bone_mass_kg REAL, visceral_fat_mass_kg REAL,
  bmd_g_cm2 REAL, t_score REAL, z_score REAL, android_fat_pct REAL, gynoid_fat_pct REAL,
  notes TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_dexa_user_time ON dexa_scans(user_id, measured_at);
CREATE TABLE user_integrations (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '{}',
  updated_at TIMESTAMP NOT NULL,
  PRIMARY KEY (user_id, provider)
);
