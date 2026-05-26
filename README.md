# STEP·SEQ — séquenceur pas-à-pas

Un séquenceur de rythmes jouable dans le navigateur, esthétique **GEM / Atari ST**,
livré comme une vraie web app **Go + Cloud Run + Supabase**.

- **24 pistes** ; instrument réglable par piste, **hauteur réglable pas à pas** (glissé vertical du pad ou flèches ↑/↓)
- **subdivision par pas** : clic droit (ou ←/→) découpe un pas en 2-4 répétitions régulières — croches, triolets, roulements
- longueur de pattern variable (4–128 pas), résolution réglable (croche → quadruple-croche)
- swing, tempo 40–300 BPM, peinture au glisser
- moteur de timing à *lookahead* calé sur l'horloge audio (pas de dérive)
- sortie **MIDI** (Web MIDI : notes + horloge 24 ppqn) et **export `.mid`**
- **entrée MIDI** : un clavier USB joue les voix, saisit la note dans le pad survolé, ou **enregistre en live** (quantisé sur le pas le plus proche pendant la lecture)
- **mode song** : enchaîne plusieurs patterns sauvegardés, chacun joué *×N* fois, avec changement de tempo/longueur à chaque pattern
- sauvegarde des patterns **et des songs** : **cloud** via Supabase quand on est connecté,
  **locale** sinon — l'app reste 100 % jouable hors-ligne

## Architecture

```
navigateur ──HTTP──► Cloud Run (binaire Go)
   │                      │
   │ supabase-js          ├─ /            frontend embarqué (go:embed)
   │ (auth, JWT)          ├─ /healthz     sonde Cloud Run
   ▼                      ├─ /config      URL + clé anon Supabase
Supabase Auth             ├─ /api/patterns   CRUD (JWT requis)
                          └─ /api/songs      CRUD (JWT requis)
                               │
                               ▼ pgx pool
                          Supabase Postgres (RLS)
```

Choix structurants :

- **Frontend mono-fichier vanilla** (`web/index.html`), embarqué dans le
  binaire via `go:embed`. La grille est sensible à la performance (tête de
  lecture rafraîchie à chaque double-croche) : basculer des classes CSS sur
  le DOM est ici plus direct et plus léger qu'un cycle de rendu React.
- **Asynchrone & perf** : pool `pgxpool` borné (8 conn. max, le *pooler*
  Supabase gère le fan-in), `context` propagé sur toute requête, timeouts,
  arrêt gracieux sur `SIGTERM`.
- **Auth** : les JWT Supabase (HS256) sont vérifiés avec la seule librairie
  standard (`crypto/hmac`) — chemin d'authentification court et auditable.
- **Défense en profondeur** : chaque requête SQL filtre par `user_id`, *et*
  la table active Row Level Security.

```
.
├── main.go                  routeur Chi, pool pgx, arrêt gracieux
├── internal/
│   ├── auth/                vérification JWT HS256 (stdlib)
│   ├── store/               accès Postgres (pgx) — patterns + songs
│   └── handlers/            API JSON patterns + songs
├── web/index.html           frontend séquenceur (embarqué)
├── supabase/schema.sql      tables (patterns, songs) + triggers + RLS
└── Dockerfile               build multi-stage, image distroless
```

## Configuration Supabase

1. Créer un projet sur [supabase.com](https://supabase.com).
2. SQL Editor → coller et exécuter `supabase/schema.sql`.
3. Authentication → Providers → activer **Email**.
4. Relever, dans *Project Settings* :
   - `DATABASE_URL` — Database → Connection string → **Transaction pooler**
     (port `6543`, recommandé pour Cloud Run).
   - `SUPABASE_JWT_SECRET` — API → JWT Settings → JWT Secret.
   - `SUPABASE_URL` et `SUPABASE_ANON_KEY` — API.

> Si le projet utilise les nouvelles *signing keys* asymétriques (ES256/RS256),
> remplacer `parse()` dans `internal/auth/auth.go` par un vérificateur JWKS.

## Développement local

```bash
cp .env.example .env        # puis renseigner les valeurs
set -a && source .env && set +a
go mod tidy                 # génère go.sum
go run .
# → http://localhost:8080
```

Sans `DATABASE_URL`, le serveur démarre quand même : le séquenceur est
jouable, seul le stockage cloud des patterns est désactivé.

## Déploiement Cloud Run

```bash
gcloud run deploy step-seq \
  --source . \
  --region europe-west1 \
  --allow-unauthenticated \
  --set-env-vars "SUPABASE_URL=https://<ref>.supabase.co" \
  --set-env-vars "SUPABASE_ANON_KEY=<anon-key>" \
  --set-secrets  "DATABASE_URL=step-seq-db:latest" \
  --set-secrets  "SUPABASE_JWT_SECRET=step-seq-jwt:latest"
```

Créer les secrets sensibles au préalable :

```bash
printf '%s' "<connection-string>" | \
  gcloud secrets create step-seq-db --data-file=-
printf '%s' "<jwt-secret>" | \
  gcloud secrets create step-seq-jwt --data-file=-
```

Cloud Run injecte `PORT` automatiquement ; `/healthz` sert de sonde.

## Esthétique

Hommage à l'environnement **GEM** de l'Atari ST : chrome gris bizeauté,
barre de titre rayée, *close box*, le tout autour d'un « écran » CRT sombre
(scanlines, vignette, animation d'allumage) où vit une grille néon à six
couleurs. Aucun logo ni marque Atari n'est utilisé — c'est un clin d'œil.

## Pistes d'extension

- patterns partagés / publics (lecture seule via la clé anon + RLS)
- export WAV hors-ligne via `OfflineAudioContext`
- accent / vélocité par pas
- ~~enchaînement de patterns (mode *song*)~~ — fait, bouton **Song** dans le pied de page
