# STEP·SEQ — séquenceur pas-à-pas

Un séquenceur de rythmes jouable dans le navigateur, esthétique **GEM / Atari ST**,
livré comme une vraie web app **Go + Cloud Run + Cloud SQL Postgres**.

- **24 pistes** ; instrument réglable par piste, **hauteur réglable pas à pas** (glissé vertical du pad ou flèches ↑/↓)
- **subdivision par pas** : clic droit (ou ←/→) découpe un pas en 2-4 répétitions régulières — croches, triolets, roulements
- longueur de pattern variable (4–128 pas), résolution réglable (croche → quadruple-croche)
- swing, tempo 40–300 BPM, peinture au glisser
- moteur de timing à *lookahead* calé sur l'horloge audio (pas de dérive)
- sortie **MIDI** (Web MIDI : notes + horloge 24 ppqn) et **export `.mid`**
- **entrée MIDI** : un clavier USB joue les voix, saisit la note dans le pad survolé, ou **enregistre en live** (quantisé sur le pas le plus proche pendant la lecture)
- **mode song** : enchaîne plusieurs patterns sauvegardés, chacun joué *×N* fois, avec changement de tempo/longueur à chaque pattern
- **synthé FM 2 opérateurs par piste** : carrier + modulateur, ratio 1/8 → 16, index 0 → 24, ADSR séparé pour l'amplitude et la modulation, 4 ondes (sine/square/saw/triangle)
- **échantillonneur** : import de fichier audio OU enregistrement micro 3 s ; lecture transposée par pitch (`playbackRate`), ADSR sur l'amplitude
- **FM appliquée au sample** : deux moteurs au choix — *detune* (oscillateur module le `detune` en cents, chorus/vibrato profond) ou *worklet* (vraie FM audio-rate via `AudioWorklet`, position dans le buffer modulée par un sinus à fréquence porteuse)
- **vélocité / accent par pas** : touches **A** (accent, vel 127) / **G** (ghost, vel 50) / **N** (normal, vel 100) au survol — l'opacité du pad reflète l'intensité, et le `noteOn` MIDI sort à la bonne velocity
- **mixer global** : gain + pan par piste, mute / solo, sauvegardé avec le pattern, appliqué en direct via un nœud `StereoPanner` par piste
- **bibliothèque d'instruments** par utilisateur : sauvegarde / rappel de presets FM ou samples, synchronisée cloud quand on est connecté
- comptes applicatifs : signup/login bcrypt + JWT maison (HS256), pas de dépendance d'auth tierce
- sauvegarde des patterns, songs **et instruments** : **cloud** (Cloud SQL) quand on est connecté,
  **locale** (`localStorage`) sinon — l'app reste 100 % jouable hors-ligne

## Architecture

```
navigateur ──HTTP──► Cloud Run (binaire Go)
   │                      │
   │ JWT Bearer           ├─ /                  frontend embarqué (go:embed)
   │                      ├─ /health            sonde Cloud Run
   │                      ├─ /config            feature flag "cloud"
   │                      ├─ /api/signup        bcrypt + token
   │                      ├─ /api/login         vérif + token
                          ├─ /api/patterns      CRUD (JWT requis)
   ▼                      ├─ /api/songs         CRUD (JWT requis)
   localStorage           └─ /api/instruments   CRUD (JWT requis)
   (fallback                    │
    hors-ligne)                 ▼ pgx pool
                          Cloud SQL Postgres
```

Choix structurants :

- **Frontend mono-fichier vanilla** (`web/index.html`), embarqué dans le
  binaire via `go:embed`. La grille est sensible à la performance (tête de
  lecture rafraîchie à chaque double-croche) : basculer des classes CSS sur
  le DOM est ici plus direct et plus léger qu'un cycle de rendu React.
- **Asynchrone & perf** : pool `pgxpool` borné (8 conn. max), `context`
  propagé sur toute requête, timeouts, arrêt gracieux sur `SIGTERM`.
- **Auth** : compte applicatif → bcrypt (`golang.org/x/crypto/bcrypt`) côté
  serveur, JWT HS256 signés avec un secret de déploiement
  (`JWT_SECRET`). Vérification *constant-time* via `hmac.Equal` ; refus
  explicite de toute autre `alg` que HS256 pour neutraliser l'attaque
  *algorithm confusion*. Code complet : `internal/auth/auth.go`.
- **Isolation des données** : chaque requête SQL filtre par `user_id` et
  les tables ont une FK `users(id) ON DELETE CASCADE`.

```
.
├── main.go                  routeur Chi, pool pgx, arrêt gracieux
├── internal/
│   ├── auth/                bcrypt + JWT HS256 (stdlib + x/crypto)
│   ├── store/               accès Postgres (pgx) — users + patterns + songs + instruments
│   └── handlers/            API JSON : signup, login, patterns, songs, instruments
├── web/index.html           frontend séquenceur (embarqué, ~80 ko de JS inline)
├── db/schema.sql            tables (users, patterns, songs, instruments) + triggers
├── docker-compose.yml       Postgres pour le dev local
└── Dockerfile               build multi-stage, image distroless
```

## Développement local

```bash
# 1) Postgres dans Docker, schéma appliqué au premier boot
docker compose up -d

# 2) variables d'environnement
cp .env.example .env       # le fichier par défaut pointe sur localhost:5432

# 3) lancer le serveur
set -a && source .env && set +a
go run .
# → http://localhost:8080
```

Sans `DATABASE_URL`, le serveur démarre quand même : le séquenceur est
jouable, seuls les comptes et le stockage cloud sont désactivés
(la banque tombe en `localStorage`).

## Déploiement Cloud Run + Cloud SQL

```bash
PROJECT=peopleofversoapp
REGION=europe-west1
INSTANCE=atari-pg

# 1) Cloud SQL (Postgres 16, smallest tier)
gcloud sql instances create $INSTANCE \
  --project=$PROJECT \
  --database-version=POSTGRES_16 \
  --tier=db-f1-micro \
  --region=$REGION \
  --root-password="$(openssl rand -base64 24)"
gcloud sql databases create atari --instance=$INSTANCE --project=$PROJECT
gcloud sql users create atari --instance=$INSTANCE --project=$PROJECT \
  --password="$(openssl rand -base64 24)"   # ⚠ noter ce mot de passe

# 2) Appliquer le schéma (via Cloud SQL Proxy ou l'editeur web)
gcloud sql connect $INSTANCE --user=postgres --project=$PROJECT < db/schema.sql

# 3) Secrets Cloud Run
printf '%s' "postgres://atari:<password>@/atari?host=/cloudsql/$PROJECT:$REGION:$INSTANCE" | \
  gcloud secrets create atari-db --data-file=- --project=$PROJECT
printf '%s' "$(openssl rand -base64 48)" | \
  gcloud secrets create atari-jwt --data-file=- --project=$PROJECT

# 4) Deploy
gcloud run deploy atari-seq \
  --project=$PROJECT \
  --source=. \
  --region=$REGION \
  --allow-unauthenticated \
  --add-cloudsql-instances=$PROJECT:$REGION:$INSTANCE \
  --set-secrets=DATABASE_URL=atari-db:latest \
  --set-secrets=JWT_SECRET=atari-jwt:latest
```

Cloud Run injecte `PORT` automatiquement ; `/health` sert de sonde
(`/healthz` est réservé par le Google Frontend sur `*.run.app` et
renverrait un 404 avant d'atteindre le container).

## Esthétique

Hommage à l'environnement **GEM** de l'Atari ST : chrome gris bizeauté,
barre de titre rayée, *close box*, le tout autour d'un « écran » CRT sombre
(scanlines, vignette, animation d'allumage) où vit une grille néon à six
couleurs. Aucun logo ni marque Atari n'est utilisé — c'est un clin d'œil.

## Pistes d'extension

- patterns partagés / publics (lecture seule via un token public)
- export WAV hors-ligne via `OfflineAudioContext`
- accent / vélocité par pas
- ~~enchaînement de patterns (mode *song*)~~ — fait, bouton **Song**
- ~~synthé FM + samples + enregistrement micro~~ — fait, bouton **EDIT** par piste dans le panneau Voix
- ~~vraie FM audio-rate via `AudioWorklet`~~ — fait, sélecteur *Moteur* dans le panneau Sample du synth editor
- ~~accent / vélocité par pas~~ — fait, touches A / G / N au survol d'un pad
- ~~mixer global (gain / pan / mute / solo)~~ — fait, bouton **Mix**
