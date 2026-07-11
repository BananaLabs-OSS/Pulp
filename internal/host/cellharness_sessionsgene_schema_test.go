package host

// Schema for the Sessions-Gene harness's seed DB. The cell's bootstrap only
// OPENS its sqlite DB (Evolution runs migrations in production), so the harness
// owns table creation. Columns mirror Sessions-Gene/pulp-cell/models.go
// bun-tag-for-bun-tag for the tables the owner-gate + getSessions paths touch,
// so bun's `SELECT *` (which lists every mapped column by name) resolves every
// column. Only the subset of tables those handlers read is created.

const sessionsGeneSchema = `
CREATE TABLE IF NOT EXISTS orders (
	id TEXT PRIMARY KEY,
	stripe_session_id TEXT NOT NULL UNIQUE,
	server_type TEXT NOT NULL,
	email TEXT,
	status TEXT NOT NULL,
	username TEXT,
	whitelist TEXT,
	settings_json TEXT,
	gamemode TEXT,
	difficulty TEXT,
	pvp TEXT,
	hardcore TEXT,
	seed TEXT,
	world_type TEXT,
	motd TEXT,
	upload_id TEXT,
	amount_cents INTEGER,
	promo_code TEXT,
	coupon_id TEXT,
	game_rules_json TEXT,
	datapack_urls TEXT,
	datapack_ids TEXT,
	engine TEXT,
	version TEXT,
	mods_json TEXT,
	extend_mode TEXT,
	extend_server_id TEXT,
	upgrade_intent_id TEXT,
	upgrade_target TEXT,
	extend_order_id TEXT,
	is_gift BOOLEAN NOT NULL DEFAULT FALSE,
	gift_token TEXT,
	gift_claimed BOOLEAN NOT NULL DEFAULT FALSE,
	gift_claimed_at TIMESTAMP,
	buyer_email TEXT,
	auto_redeem BOOLEAN NOT NULL,
	voucher_expires_at TIMESTAMP,
	scheduled_at TIMESTAMP,
	original_server_type TEXT,
	max_amount_cents INTEGER NOT NULL DEFAULT 0,
	last_nudge_at TIMESTAMP,
	returnable_until TIMESTAMP,
	parent_order_id TEXT,
	tier_id TEXT,
	reconfigure_pending BOOLEAN NOT NULL DEFAULT FALSE,
	is_public BOOLEAN NOT NULL DEFAULT FALSE,
	fraud_reasons TEXT,
	resolve_cache_json TEXT NOT NULL DEFAULT '',
	eu_waiver_accepted_at TIMESTAMP,
	ip_address TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS servers (
	id TEXT PRIMARY KEY,
	order_id TEXT NOT NULL,
	container_id TEXT,
	server_name TEXT,
	ip TEXT,
	port INTEGER,
	ports_json TEXT,
	template TEXT NOT NULL,
	state TEXT NOT NULL,
	expires_at TIMESTAMP,
	promoted_at TIMESTAMP,
	warned_at TIMESTAMP,
	created_at TIMESTAMP NOT NULL,
	destroyed_at TIMESTAMP,
	extends_server_id TEXT,
	upload_id TEXT,
	restart_count INTEGER NOT NULL DEFAULT 0,
	whitelist_json TEXT,
	ops_json TEXT,
	bans_json TEXT,
	cpu_weight REAL NOT NULL DEFAULT 0.33,
	memory_weight REAL NOT NULL DEFAULT 3,
	operating BOOLEAN NOT NULL DEFAULT FALSE,
	operating_since TIMESTAMP,
	settings_json TEXT,
	auto_restart TEXT,
	last_auto_restart TIMESTAMP,
	display_name TEXT,
	final_warned_at TIMESTAMP,
	share_token TEXT,
	locked_until TIMESTAMP,
	paused_at TIMESTAMP,
	total_paused_ms INTEGER NOT NULL DEFAULT 0,
	game_id TEXT
);

CREATE TABLE IF NOT EXISTS claim_tokens (
	id TEXT PRIMARY KEY,
	email TEXT NOT NULL,
	token TEXT NOT NULL UNIQUE,
	claimed BOOLEAN NOT NULL DEFAULT FALSE,
	expires_at TIMESTAMP NOT NULL,
	created_at TIMESTAMP NOT NULL,
	ip_address TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pools (
	id TEXT PRIMARY KEY,
	pool_token TEXT NOT NULL UNIQUE,
	name TEXT,
	server_type TEXT NOT NULL,
	target_cents INTEGER NOT NULL,
	collected_cents INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL,
	creator_email TEXT NOT NULL,
	order_id TEXT,
	extend_server_id TEXT,
	extend_order_id TEXT,
	tier_id TEXT,
	expires_at TIMESTAMP NOT NULL,
	created_at TIMESTAMP NOT NULL,
	settings_json TEXT,
	gamemode TEXT,
	difficulty TEXT,
	pvp TEXT,
	hardcore TEXT,
	seed TEXT,
	world_type TEXT,
	motd TEXT,
	upload_id TEXT,
	game_rules_json TEXT,
	datapack_urls TEXT,
	promo_code TEXT,
	coupon_id TEXT
);

CREATE TABLE IF NOT EXISTS pool_contributions (
	id TEXT PRIMARY KEY,
	pool_id TEXT NOT NULL,
	username TEXT NOT NULL,
	email TEXT NOT NULL,
	amount_cents INTEGER NOT NULL,
	stripe_pi TEXT,
	voucher_order_id TEXT,
	confirmed BOOLEAN NOT NULL DEFAULT FALSE,
	captured BOOLEAN NOT NULL DEFAULT FALSE,
	is_creator BOOLEAN NOT NULL DEFAULT FALSE,
	anonymous BOOLEAN NOT NULL DEFAULT FALSE,
	platform TEXT,
	created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS queue (
	id TEXT PRIMARY KEY,
	order_id TEXT NOT NULL UNIQUE,
	position INTEGER NOT NULL,
	created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS reconfiguration_log (
	id TEXT PRIMARY KEY,
	server_id TEXT NOT NULL,
	order_id TEXT NOT NULL,
	from_state TEXT,
	to_state TEXT,
	price_delta_cents INTEGER NOT NULL DEFAULT 0,
	charged_to TEXT,
	outcome TEXT NOT NULL DEFAULT 'succeeded',
	created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS game_visibility (
	template TEXT PRIMARY KEY,
	game_id TEXT NOT NULL,
	label TEXT,
	enabled BOOLEAN NOT NULL DEFAULT FALSE,
	hidden BOOLEAN NOT NULL DEFAULT FALSE,
	engine TEXT NOT NULL DEFAULT '',
	price_override INTEGER,
	duration_override TEXT,
	max_players_override INTEGER,
	tagline_override TEXT,
	tags_override_json TEXT,
	description_override TEXT,
	max_instances INTEGER,
	extend_instant_pct INTEGER,
	extend_queued_pct INTEGER,
	config_json TEXT,
	tier_id TEXT
);

CREATE TABLE IF NOT EXISTS tiers (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	label TEXT NOT NULL,
	price_cents INTEGER NOT NULL DEFAULT 0,
	duration TEXT NOT NULL,
	extend_instant_pct INTEGER NOT NULL DEFAULT 75,
	extend_queued_pct INTEGER NOT NULL DEFAULT 50,
	max_cpu REAL NOT NULL DEFAULT 0,
	max_ram_mb INTEGER NOT NULL DEFAULT 0,
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	sort_order INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL
);
`
