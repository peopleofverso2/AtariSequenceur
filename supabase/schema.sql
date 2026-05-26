-- ============================================================
-- STEP·SEQ — schéma Supabase
-- À exécuter dans l'éditeur SQL Supabase, ou via la CLI :
--   supabase db push
-- ============================================================

create extension if not exists pgcrypto;

create table if not exists public.patterns (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null references auth.users (id) on delete cascade,
    name        text not null check (char_length(name) between 1 and 60),
    bpm         integer not null default 120 check (bpm between 40 and 300),
    swing       integer not null default 0   check (swing between 0 and 75),
    steps       jsonb   not null default '{}'::jsonb,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now()
);

-- Liste « mes patterns, plus récents d'abord » : index dédié.
create index if not exists patterns_user_updated_idx
    on public.patterns (user_id, updated_at desc);

-- Maintient updated_at à jour à chaque écriture.
create or replace function public.set_updated_at()
returns trigger
language plpgsql
as $$
begin
    new.updated_at = now();
    return new;
end;
$$;

drop trigger if exists patterns_set_updated_at on public.patterns;
create trigger patterns_set_updated_at
    before update on public.patterns
    for each row execute function public.set_updated_at();

-- Row Level Security : le backend Go filtre déjà par user_id, mais RLS
-- protège la table si elle est atteinte via l'API PostgREST (clé anon).
alter table public.patterns enable row level security;

drop policy if exists "patterns are private to their owner" on public.patterns;
create policy "patterns are private to their owner"
    on public.patterns
    for all
    using (auth.uid() = user_id)
    with check (auth.uid() = user_id);

-- ============================================================
-- Songs : suite ordonnée de patterns (mode « song »)
-- items est un tableau JSON [{ "patternId": "...", "repeats": 1 }, ...]
-- patternId peut référencer un pattern cloud (uuid) ou local (préfixe
-- "loc-"), donc on ne pose pas de FK : la résolution se fait côté client.
-- ============================================================
create table if not exists public.songs (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null references auth.users (id) on delete cascade,
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

alter table public.songs enable row level security;

drop policy if exists "songs are private to their owner" on public.songs;
create policy "songs are private to their owner"
    on public.songs
    for all
    using (auth.uid() = user_id)
    with check (auth.uid() = user_id);
