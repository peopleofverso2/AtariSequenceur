-- ============================================================
-- STEP·SEQ — schéma Postgres (Cloud SQL + dev local Docker).
--   psql "$DATABASE_URL" -f supabase/schema.sql
-- ============================================================

create extension if not exists pgcrypto;

-- ============================================================
-- users : compte applicatif. Le mot de passe est haché bcrypt
-- côté Go (golang.org/x/crypto/bcrypt) avant insertion.
-- ============================================================
create table if not exists public.users (
    id            uuid primary key default gen_random_uuid(),
    email         text not null unique check (email = lower(email) and char_length(email) between 3 and 254),
    password_hash text not null,
    created_at    timestamptz not null default now()
);

-- ============================================================
-- updated_at : trigger générique réutilisable.
-- ============================================================
create or replace function public.set_updated_at()
returns trigger
language plpgsql
as $$
begin
    new.updated_at = now();
    return new;
end;
$$;

-- ============================================================
-- patterns : grille sauvegardée. steps porte la grille complète
-- (jsonb) — voir snapshot()/applyPattern() côté frontend.
-- ============================================================
create table if not exists public.patterns (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null references public.users (id) on delete cascade,
    name        text not null check (char_length(name) between 1 and 60),
    bpm         integer not null default 120 check (bpm between 40 and 300),
    swing       integer not null default 0   check (swing between 0 and 75),
    steps       jsonb   not null default '{}'::jsonb,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now()
);

create index if not exists patterns_user_updated_idx
    on public.patterns (user_id, updated_at desc);

drop trigger if exists patterns_set_updated_at on public.patterns;
create trigger patterns_set_updated_at
    before update on public.patterns
    for each row execute function public.set_updated_at();

-- ============================================================
-- songs : suite ordonnée de patterns (mode « song »).
-- items est un tableau JSON [{ "patternId": "...", "repeats": 1 }, ...].
-- patternId peut référencer un pattern cloud (uuid) ou local
-- (préfixe "loc-"), donc on ne pose pas de FK : la résolution
-- se fait côté client.
-- ============================================================
create table if not exists public.songs (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null references public.users (id) on delete cascade,
    name        text not null check (char_length(name) between 1 and 60),
    items       jsonb not null default '[]'::jsonb,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now()
);

create index if not exists songs_user_updated_idx
    on public.songs (user_id, updated_at desc);

drop trigger if exists songs_set_updated_at on public.songs;
create trigger songs_set_updated_at
    before update on public.songs
    for each row execute function public.set_updated_at();

-- ============================================================
-- instruments : bibliothèque de voix (presets FM ou échantillons)
-- réutilisables d'un pattern à l'autre. config porte la
-- configuration JSON (type fm/sample, ADSR, ratio, index, données
-- du sample base64, etc.). Les patterns continuent à embarquer
-- leurs propres voix : la bibliothèque sert à recharger un preset.
-- ============================================================
create table if not exists public.instruments (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null references public.users (id) on delete cascade,
    name        text not null check (char_length(name) between 1 and 60),
    config      jsonb not null default '{}'::jsonb,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now()
);

create index if not exists instruments_user_updated_idx
    on public.instruments (user_id, updated_at desc);

drop trigger if exists instruments_set_updated_at on public.instruments;
create trigger instruments_set_updated_at
    before update on public.instruments
    for each row execute function public.set_updated_at();

-- ============================================================
-- shares : invitation à collaborer sur une ressource (song pour
-- l'instant). Un share peut être "pending" (invitee_id NULL,
-- accepted_at NULL) lorsqu'on invite un email sans compte ; il est
-- résolu lors du signup/login si l'email correspond.
--
-- L'unicité (resource_type, resource_id, lower(invitee_email)) évite
-- de spammer plusieurs invitations identiques pour la même song.
-- ============================================================
create table if not exists public.shares (
    id            uuid primary key default gen_random_uuid(),
    resource_type text not null check (resource_type in ('song')),
    resource_id   uuid not null,
    owner_id      uuid not null references public.users (id) on delete cascade,
    invitee_id    uuid references public.users (id) on delete cascade,
    invitee_email text not null check (char_length(invitee_email) between 3 and 254),
    role          text not null default 'editor' check (role in ('viewer','editor')),
    token         text not null unique,
    created_at    timestamptz not null default now(),
    accepted_at   timestamptz
);

create unique index if not exists shares_resource_invitee_unique
    on public.shares (resource_type, resource_id, lower(invitee_email));
create index if not exists shares_invitee_idx
    on public.shares (invitee_id) where invitee_id is not null;
create index if not exists shares_owner_idx
    on public.shares (owner_id);
create index if not exists shares_pending_email_idx
    on public.shares (lower(invitee_email)) where invitee_id is null;
