---
type: index
repo: s3-webx
workspace: baunach
tenant: baunach
---

# s3-webx Repo-Vault

Repo-spezifische Doku fuer `s3-webx`: ein Go-Dienst, der S3-kompatible Buckets regelmaessig durchgeht und neben
jedes Bild eine WebP-Fassung (`foto.jpg.webp`) und neben jedes Video eine WebM-Fassung (`film.mp4.webm`) legt.

## Inhalt

- `concepts/` — Funktionsweise des Dienstes (TBD)
- `decisions/` — Repo-spezifische Entscheidungen (TBD)
- `playbooks/` — Betrieb und Erprobung (TBD)
- `troubleshooting/` — Fehlerbilder bei Umwandlung und Upload (TBD)

## Cross-Repo-Themen → Workspace-Vault

Liegt unter `~/workspaces/baunach/vault/`:

- Entwurf und Entscheidungen des Dienstes: `plans/2026-10-29-content-webx-dienst.md`
- Image-Konventionen (Registry, Tags, Architekturen, CI): `concepts/docker/postgres/image-konventionen.md`
