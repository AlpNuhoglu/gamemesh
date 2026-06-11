-- Development seed data. Password for every seeded player is "password123"
-- (bcrypt cost 12). NEVER load this in production.
INSERT INTO players (id, username, email, password_hash)
VALUES
    ('11111111-1111-1111-1111-111111111111', 'alice',   'alice@example.com',   '$2a$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW'),
    ('22222222-2222-2222-2222-222222222222', 'bob',     'bob@example.com',     '$2a$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW'),
    ('33333333-3333-3333-3333-333333333333', 'charlie', 'charlie@example.com', '$2a$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW'),
    ('44444444-4444-4444-4444-444444444444', 'diana',   'diana@example.com',   '$2a$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW')
ON CONFLICT (id) DO NOTHING;

INSERT INTO player_stats (player_id, rank, score, games_played)
VALUES
    ('11111111-1111-1111-1111-111111111111', 1200, 4500, 42),
    ('22222222-2222-2222-2222-222222222222', 1150, 3800, 35),
    ('33333333-3333-3333-3333-333333333333',  980, 2100, 18),
    ('44444444-4444-4444-4444-444444444444', 1010, 2600, 23)
ON CONFLICT (player_id) DO NOTHING;
