---
type: concept
repo: s3-webx
status: active
tags: [s3, webp, webm, vips, ffmpeg, sqlite]
---

# Funktionsweise — s3-webx

`s3-webx` ist ein einzelnes Go-Binary. Es geht einen oder mehrere S3-kompatible Buckets in festen Abstaenden durch
und legt neben jedes Bild eine WebP-Fassung und neben jedes Video eine WebM-Fassung. Originale werden nie veraendert
und nie geloescht, auch keine verwaisten Varianten. Entwurf und Entscheidungen des Users stehen im Workspace-Plan
`vault/plans/2026-10-29-content-webx-dienst.md`; diese Note beschreibt, wie der Code es umsetzt.

## Pakete

| Paket | Aufgabe |
|---|---|
| `cmd/s3-webx` | Verdrahtung: Konfiguration laden, Zustand oeffnen, je Bucket einen Store und eine Scan-Schleife, zwei Arbeiter, Signale |
| `internal/config` | YAML einlesen, `${NAME}` aus der Umgebung ersetzen, Defaults je Bucket feldweise ueberschreiben, pruefen |
| `internal/state` | SQLite-Zustand `state.db`: Warteschlange, Ergebnis, Versuche je Quellversion, Abfragen fuer die HTTP-Schicht |
| `internal/store` | schmaler S3-Typ ueber `minio-go/v7`: Liste, Herunterladen, Hochladen, Cache-Control eines Objekts |
| `internal/convert` | `vips` fuer Bilder, `ffmpeg`/`ffprobe` fuer Videos, jeweils per `exec` |
| `internal/worker` | Benennung, Entscheidung je Original, Scan-Schleife, die beiden Arbeiter mit ihrem Fortschritt |
| `internal/web` | HTTP-Schicht: `/healthz`, HTML-Uebersicht, JSON-API, Basic- und Bearer-Pruefung |

`internal/worker` kennt S3 und die Konverter nur ueber eigene Schnittstellen (`Store`, `Converter`). Im Test stehen
dort handgeschriebene Doubles (`doubles_test.go`); den Zustand pruefen die Tests gegen echtes SQLite in einer
Temp-Datei, weil ein Double dafuer mehr Code waere als die Abfragen selbst.

## Ablauf

```
Scanner je Bucket (alle `interval`)            Arbeiter image / video (je einer, sequentiell)
  List ──▶ KindOf ──▶ Decide ──▶ Enqueue ──▶ Wake ──▶ Claim (aelteste zuerst, alle Buckets)
                                                    ──▶ Download nach /data/tmp/job-*/source.<ext>
                                                    ──▶ vips | ffmpeg ──▶ variant.webp | variant.webm
                                                    ──▶ nicht kleiner und keep_if_larger aus? → skipped, kein Upload
                                                    ──▶ sonst Upload <key>.webp | <key>.webm → done
```

- **Scan:** Je Bucket laeuft eine Schleife, die sofort beim Start und danach im Intervall des Buckets (`interval`,
  sonst das globale) alle Objekte unter `prefix` listet. Fuer jedes Original entscheidet `Decide`, ob es umgewandelt
  werden muss; Kandidaten traegt der Scanner als `queued` in `state.db` ein und weckt den passenden Arbeiter.
- **Arbeiter:** Es gibt genau zwei, `image` und `video`, unabhaengig von der Zahl der Buckets. Jeder nimmt seine Art
  nacheinander ueber alle Buckets ab, und zwar den am laengsten wartenden Eintrag zuerst (`queued_at`, bei Gleichstand
  das aeltere Original). Ein langes Video haelt so keine Bilder auf. Ohne Arbeit wartet ein Arbeiter, bis ein Scanner
  ihn weckt.
- **Video aus:** Ein Bucket mit `video.enabled: false` liefert keine Video-Kandidaten, und der Video-Arbeiter bedient
  ihn auch nicht — so bleiben Eintraege aus einer frueheren Konfiguration liegen, statt umgewandelt zu werden.
- **Ergebnis:** `keep_if_larger` steht per Default auf `true`: die Variante wird immer hochgeladen und der Eintrag
  heisst `done`, auch wenn sie groesser ist; die Protokollzeile zeigt beide Groessen. Ist die Option aus und die
  Variante nicht kleiner als das Original, wird sie nicht hochgeladen, der Eintrag heisst `skipped` und traegt die
  gemessene Groesse. Sonst Upload mit `Content-Type` `image/webp` bzw.
  `video/webm`; `Cache-Control` kommt bei `inherit` per `StatObject` vom Original, sonst ist es der feste Wert.
- **Arbeitsordner:** `tmp/` neben `state.db`, also `/data/tmp` auf dem Volume, damit grosse Videos nicht im
  Container-Dateisystem landen. Jede Datei bekommt einen eigenen Ordner `job-*`, der danach immer entfernt wird.
  Beim Start loescht der Dienst nur Ordner `job-*`, die ein abgebrochener Lauf liegen gelassen hat, nichts anderes.
- **Abbruch:** SIGTERM oder SIGINT beenden den Kontext. ffmpeg und vips laufen ueber `exec.CommandContext` und werden
  dabei beendet; der laufende Eintrag geht zurueck auf `queued`, ohne einen Versuch zu zaehlen. Der Dienst wartet, bis
  Scanner und Arbeiter zurueckgekehrt sind.
- **Protokoll:** `log/slog` als JSON auf stdout. Je Datei eine Zeile `file` mit `bucket`, `key`, `kind`, `result`,
  `bytes_before`, `bytes_after`, `duration` und bei Fehlern `error`; je Scan eine Zeile `scan done`.

## Benennung

- Die Variante haengt ihre Endung an den vollen Schluessel: `foto.jpg` → `foto.jpg.webp`, `film.mp4` → `film.mp4.webm`.
  Gross- und Kleinschreibung bleibt im Namen erhalten (`FOTO.JPG` → `FOTO.JPG.webp`).
- Ob ein Objekt Bild oder Video ist, entscheidet nur die Endung, ohne Ruecksicht auf Gross- und Kleinschreibung,
  gegen die Listen `image.formats` und `video.formats` des Buckets.
- Objekte, die auf `.webp` oder `.webm` enden, sind nie Eingaben, auch wenn eine Formatliste sie nennen wuerde.
  Objekte ohne Endung und Ordner-Platzhalter (Schluessel mit `/` am Ende) werden uebergangen.

## Zustandsmodell

`state.db` (SQLite ueber `modernc.org/sqlite`, ohne cgo, WAL) haelt je `(bucket, key)` eine Zeile: Art, Status,
LastModified und Groesse des Originals (zusammen die **Quellversion**), Groesse der Variante, Versuche, Fehlertext,
Dauer, `queued_at` und `updated_at`.

```
          Enqueue                Claim                MarkDone / MarkSkipped / MarkFailed
(neu) ──────────────▶ queued ─────────────▶ running ─────────────────────────────────▶ done | skipped | failed
                        ▲                     │
                        └──── Requeue ────────┘   (Shutdown; beim Start auch jedes liegengebliebene running)
```

- **Entscheidung je Original** (`worker.Decide`, reine Funktion): umwandeln, wenn die Variante fehlt oder ihr
  LastModified aelter ist als das des Originals — ausser `state.db` kennt fuer genau diese Quellversion schon
  `failed` mit drei Versuchen (`state.MaxAttempts`), oder `skipped` bei `keep_if_larger: false`. Mit
  `keep_if_larger: true` plant `Decide` einen `skipped`-Eintrag derselben Quellversion erneut ein.
- **Versuche** zaehlen je Quellversion. `Enqueue` mit einer neuen Quellversion setzt sie auf 0 und leert den
  Fehlertext; mit derselben Quellversion bleiben sie stehen, ein fehlgeschlagener Eintrag wird also beim naechsten
  Scan erneut versucht, bis drei Versuche erreicht sind.
- **Laufender Eintrag:** `Enqueue` laesst einen `queued`- oder `running`-Eintrag derselben Quellversion unberuehrt.
  Taucht waehrend einer Umwandlung eine neue Quellversion auf, wird der Eintrag wieder `queued` mit der neuen Version;
  das Ergebnis des laufenden Arbeiters greift nur, solange die Version noch zu ihm passt. So wird die neue Fassung
  danach erneut umgewandelt, obwohl die gerade hochgeladene Variante juenger ist als das neue Original.
- **Leeres Original:** Ein Objekt mit 0 Byte verbucht der Arbeiter sofort als `failed` mit `source object is empty
  (0 bytes)` und `state.MaxAttempts` Versuchen (`state.MarkGivenUp`), ohne Download und ohne vips/ffmpeg; erst eine
  neue Quellversion plant es wieder ein. Anlass war in Produktion ein 0-Byte-JPEG, das vips mit `is not a known file
  format` ablehnte.
- **Start:** `state.Open` setzt jeden `running`-Eintrag auf `queued`, weil beim Start nichts laufen kann.
- Zeitpunkte liegen als Unix-Nanosekunden in `INTEGER`-Spalten, die Dauer in Millisekunden.

## Konfiguration

Eine YAML-Datei, Pfad per `-config` (Default `/config/s3-webx.yaml`); Beispiel mit allen Schluesseln:
`config.example.yaml` im Repo.

- **Strikt:** Unbekannte Schluessel sind ein Fehler (`yaml.v3` mit `KnownFields(true)`), auch innerhalb eines Buckets.
  Damit faellt ein Tippfehler wie `max_egde` beim Start auf, statt still den Default zu nehmen.
- **Secrets:** `${NAME}` wird aus der Umgebung ersetzt. Ersetzt wird im geparsten YAML-Baum und nicht im Rohtext; ein
  Wert mit `:`, `#` oder Anfuehrungszeichen kann das Dokument so nicht verbiegen. Fehlt eine Variable, bricht der Start
  ab, und die Meldung nennt alle fehlenden Namen auf einmal.
- **Ueberschreiben:** `defaults` gilt fuer alle Buckets; ein Bucket ueberschreibt einzelne Felder von `image`, `video`,
  `keep_if_larger`, `cache_control` und `interval`, der Rest bleibt vom Default. Eine Formatliste ersetzt die Liste des
  Defaults im Ganzen. Formate werden klein geschrieben und ein fuehrender Punkt entfernt (`.PNG` → `png`).
- **Pruefung** (alle Fehler in einer Meldung): mindestens ein Bucket, Name Pflicht und eindeutig, `endpoint`, `bucket`,
  `access_key` und `secret_key` Pflicht, Intervalle > 0, `max_edge` und `max_height` > 0, `quality` 1–100, `crf`
  0–63, `audio_bitrate` der Form `96k`, `cache_control` nicht leer, nur bekannte Formate (Bilder `jpg`, `jpeg`, `png`,
  `gif`, `tif`, `tiff`; Videos `mp4`, `mov`, `m4v`, `mkv`, `avi`).
- **Immer eine Variante** (Entscheidung des Users, 2026-10-29): `keep_if_larger` steht per Default auf `true`. Fuer
  die konfigurierten Buckets entsteht so zu jedem Original eine `.webp` bzw. `.webm`, auch wenn sie groesser ist —
  die Webseite soll ohne Nachfrage beim Dienst `.webp`/`.webm` verlinken koennen. `false` bleibt je Bucket oder in
  `defaults` moeglich und behaelt das fruehere Verhalten fuer fremde Nutzer des oeffentlichen Images.
- **Alte `skipped`-Eintraege:** Eintraege, die vor der Umstellung als `skipped` verbucht wurden, plant `Decide` bei
  `keep_if_larger: true` beim naechsten Scan erneut ein (`Enqueue` setzt sie auf `queued`); sie bekommen ihre Variante
  ohne Eingriff in `state.db`. Bei `false` bleiben sie liegen wie bisher.
- `auth` (`ui_user`, `ui_password`, `api_token`) und `listen` nutzt die HTTP-Schicht (siehe dort).
- **Endpoint:** mit Schema (`https://…`, `http://minio:9000`); ohne Schema gilt `https`. `path_style: true` schaltet
  minio-go auf Pfad-Adressierung (MinIO, Hetzner), sonst virtuelle Hosts (R2, AWS).

## Bilder: `vips`

```
vips thumbnail source.jpg 'variant.webp[Q=80,strip]' 1600 --height 1600 --size down
vips thumbnail 'source.gif[n=-1]' 'variant.webp[Q=80,strip]' 1600 --height 1600 --size down
```

- `thumbnail` wertet die EXIF-Ausrichtung selbst aus und dreht das Bild; `cwebp` koennte das nicht.
- Breite und Hoehe 1600 als Kasten begrenzen die **lange** Kante auf `max_edge`, gleich ob quer oder hochkant.
- `--size down` vergroessert nie.
- `[n=-1]` laedt bei GIF alle Einzelbilder; `webpsave` schreibt daraus ein animiertes WebP mit den Bildzeiten des GIF.
  Nur fuer `.gif` gesetzt, weil die Option bei Einzelbild-Formaten nichts bringt.
- **`strip` wirkt in vips 8.14 bei `webpsave` nicht:** Der EXIF-Block des Originals (Kamera, GPS) landet trotzdem in
  der WebP-Datei. Deshalb entfernt der Konverter danach selbst die RIFF-Chunks `EXIF` und `XMP ` und loescht die
  zugehoerigen Bits im `VP8X`-Kopf (`stripWebPMetadata`). Das ICC-Profil bleibt, damit Farben stimmen. Steigt das
  Image auf eine vips-Version, in der `strip` (bzw. `keep=none`) greift, kann der Schritt bleiben; er findet dann nichts.
- Transparenz bleibt erhalten (PNG, GIF).

Gemessen am 2026-10-29 im Devcontainer (vips 8.14.1) an erzeugten Testdateien:

| Eingabe | vorher | nachher |
|---|---|---|
| JPEG 4000×3000, EXIF-Orientation 6, Kamera-Tag | 9.472.360 B | 1200×1600 (hochkant, richtig gedreht), 687.692 B, ohne EXIF |
| PNG 300×200 | — | 300×200, nicht vergroessert |
| GIF 640×360, 20 Einzelbilder | 254.902 B | animiertes WebP, 20 Einzelbilder, 147.640 B |

## Videos: `ffmpeg`

```
ffprobe -v error -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 source.mp4
ffmpeg -nostdin -hide_banner -loglevel error -nostats -y -i source.mp4 \
  -map 0:v:0 -map '0:a:0?' -map_metadata -1 \
  -vf "scale=w='if(lte(iw,ih),trunc(min(iw,1080)/2)*2,-2)':h='if(lte(iw,ih),-2,trunc(min(ih,1080)/2)*2)'" \
  -pix_fmt yuv420p -c:v libvpx-vp9 -crf 33 -b:v 0 -row-mt 1 -deadline good -cpu-used 2 \
  -c:a libopus -b:a 96k -progress pipe:1 -f webm variant.webm
```

- **Masse:** Die **kuerzere** Kante wird auf hoechstens `max_height` begrenzt, damit ein Hochkant-Video 1080×1920
  bleibt und nicht auf 608×1080 schrumpft. ffmpeg dreht ein Handy-Video mit Rotations-Metadatum vor dem Filter selbst
  aufrecht, `iw`/`ih` sind also schon die angezeigten Masse. `min(…)` skaliert nie hoch, `trunc(…/2)*2` und `-2`
  halten beide Kanten gerade.
- **Spuren:** genau die erste Video- und, falls vorhanden, die erste Tonspur (`0:a:0?`). Ein Video ohne Ton laeuft
  so durch, und Daten- oder Timecode-Spuren aus `.mov`, die WebM nicht tragen kann, fallen weg. `-map_metadata -1`
  entfernt Metadaten wie Aufnahmeort.
- **`-pix_fmt yuv420p`:** 10-Bit- oder 4:4:4-Quellen (iPhone-HEVC) ergaeben sonst VP9-Profile, die nicht jeder Browser
  abspielt.
- **Qualitaet:** `-crf <crf> -b:v 0` ist der konstante Qualitaetsmodus von libvpx-vp9; `-row-mt 1` nutzt mehrere
  Kerne. `-deadline good -cpu-used 2` ist eigene Wahl: der Default `cpu-used 0` ist um ein Vielfaches langsamer bei
  kaum sichtbarem Gewinn, und der Video-Arbeiter arbeitet sequentiell.
- **Fortschritt:** `ffprobe` liefert die Dauer, `-progress pipe:1` schreibt laufend `out_time_us=`; daraus ergibt sich
  der Prozentwert, den der Arbeiter threadsicher bereithaelt. Ohne bekannte Dauer laeuft die Umwandlung ohne
  Fortschritt.

Gemessen am 2026-10-29 (ffmpeg 5.1.9) an erzeugten Testvideos (`testsrc2`, H.264):

| Eingabe | vorher | nachher |
|---|---|---|
| 2560×1440, 4 s, mit Ton | 5.351.126 B | VP9 1920×1080 + Opus, 2.447.605 B |
| 1440×2560 `.mov`, 3 s, ohne Ton | 4.243.673 B | VP9 1080×1920, 1.780.001 B |
| 1920×1080 mit Rotation 90° | — | VP9 1080×1920 (aufrecht) |
| 641×361 `.m4v`, yuv444p | 297.686 B | VP9 640×360, 257.532 B |
| 1920×1080, 3 s, ohne Ton | 2.401.091 B | 2.405.842 B — nicht kleiner, `skipped` nur bei `keep_if_larger: false` |

## HTTP-Schicht

`internal/web` ist ein `http.Handler` ueber `net/http.ServeMux` mit Go-1.22-Mustern, ohne weitere Abhaengigkeit.
`main.go` bindet `listen` vor dem Start von Scannern und Arbeitern, damit ein belegter Port den Start sofort abbricht,
und faehrt den `http.Server` bei SIGTERM mit `Shutdown` und fuenf Sekunden Frist herunter, bevor es auf Scanner und
Arbeiter wartet. Zeitgrenzen: `ReadHeaderTimeout` 5 s, `ReadTimeout` 10 s, `WriteTimeout` 30 s, `IdleTimeout` 60 s.

| Endpunkt | Zugang | Inhalt |
|---|---|---|
| `GET /healthz` | offen | `200 ok`, wenn `state.db` antwortet (`state.DB.Ping`, 2 s Frist), sonst `503` |
| `GET /` | Basic | HTML-Uebersicht aus `overview.html` (per `embed`), `<meta http-equiv="refresh" content="10">`, `?recent=N&failures=M` |
| `GET /assets/app.css` | Basic | Stylesheet der Uebersicht (per `embed`), `text/css`, ein Jahr Cache |
| `GET /api/status` | Bearer | je Arbeiter laufende Datei oder `idle`, je Bucket Zaehler, letzter Scan, Bytes |
| `GET /api/buckets/{name}/files` | Bearer | Eintraege eines Buckets, Filter `status`, `kind`, `limit`, `offset`, mit `total` |
| `GET /api/buckets/{name}/files/{key...}` | Bearer | eine Datei mit `variantKey` und `variantAvailable` |
| jeder andere Pfad unter `/api/` | Bearer | `404` als JSON |

- **Zugang:** Benutzer, Passwort und Token werden ueber `crypto/subtle.ConstantTimeCompare` verglichen, vorher beide
  Seiten mit SHA-256 auf gleiche Laenge gebracht. Benutzer und Passwort werden immer beide geprueft. Ein leeres Secret
  sperrt den Zugang mit `403`, bevor ueberhaupt Zugangsdaten gelesen werden; offen ist er nie. Fehlende oder falsche
  Daten: `401` mit `WWW-Authenticate: Basic realm="s3-webx", charset="UTF-8"` bzw. `Bearer realm="s3-webx"`.
- **Protokoll:** kein Eintrag je Anfrage, nur Fehler (Datenbank, Template, `http.Server.ErrorLog`). Secrets und der
  `Authorization`-Header landen nie im Log.
- **Arbeiterstand:** Die Schicht kennt Arbeiter und Scanner nur ueber die Schnittstelle `web.Activity`
  (`Current(kind)`, `LastScan(bucket)`). `main.go` setzt sie mit den echten `worker.Worker` und `worker.Scanner`
  um; im Test steht dort ein handgeschriebenes Double.
- **Letzter Scan:** `worker.Scanner.LastScan()` haelt Zeitpunkt und Fehler des letzten beendeten Scans, auch eines
  fehlgeschlagenen. Ein Scan, den der Shutdown abbricht, wird nicht festgehalten. Der Wert lebt nur im Speicher; nach
  einem Neustart ist er `null`, bis der sofortige erste Scan durch ist.
- **Bytes:** `state.DB.Totals` summiert je Bucket Original- und Variantengroesse nur der `done`-Eintraege, denn nur
  deren Variante liegt im Bucket. `bytesSaved` ist die Differenz. Mit `keep_if_larger: true` kann sie negativ sein.
- **Liste:** `state.DB.List` nimmt einen `state.Filter` (Bucket, Art, mehrere Status) und liefert die Seite und die
  Gesamtzahl aus einer zweiten `COUNT`-Abfrage, damit `total` auch hinter der letzten Seite stimmt.
- **Schluessel im Pfad:** `{key...}` nimmt den Rest des Pfads mit allen `/`. Der Client kodiert jedes Segment
  (`Foto%201.JPG`, `%23` fuer `#`, `%3F` fuer `?`). `ServeMux` bereinigt Pfade mit `//`, `.` oder `..` per Umleitung;
  ein Schluessel mit solchen Segmenten ist ueber diesen Endpunkt nicht erreichbar.
- **Uebersicht:** serverseitig gerendert, kein JavaScript, im Stil der Kontingent-Ansicht von `bis-status-page`
  (Entwurf: Workspace-Vault `vault/artifacts/2026-10-29-s3-webx-overview-statuspage-stil/`), hoechstens 1456 px breit.
  Von oben: Kopf mit Zustand `Converting`/`Idle`, Karte „Storage saved“ ueber alle Buckets mit Balken Originale und
  Varianten, Kacheln Queued/Done/Skipped/Failed, je Arbeiter eine Karte (Datei, Laufzeit, bei Video Prozent mit
  Balken), je Bucket eine Karte mit letztem Scan, Scanfehler, Bytes und Tabelle „Jobs by kind“ (ab zwei Buckets
  zweispaltig ab 1024 px), dann „Recent conversions“ (`done`, `skipped`) und „Failures“. Hell und dunkel ueber
  `prefers-color-scheme`. Unter 720 px wird jede Tabellenzeile ein Block mit Beschriftung aus `data-label`, nichts
  scrollt waagerecht. Status steht immer als Wort neben Farbe und Symbol.
- **Blaettern:** beide Tabellen je 25 Zeilen, `?recent=N&failures=M`. `state.DB.List` liefert die Gesamtzahl; eine
  nicht lesbare oder zu kleine Zahl ist Seite 1, eine zu grosse die letzte Seite (dann zweite Abfrage), nie ein Fehler.
  Die Links „Newer“/„Older“ tragen die Seite der anderen Tabelle mit und springen per `#recent`/`#failures` zur Karte;
  hat eine Tabelle nur eine Seite, entfaellt ihre Leiste.
- **CSS und CSP:** `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self' data:;
  frame-ancestors 'none'` — kein `unsafe-inline`, die Seite traegt kein `style=`. Die Balken sind deshalb
  `<progress>`-Elemente mit Wert (Variante gekappt auf 0–100), gefaerbt ueber `::-webkit-progress-value` und
  `::-moz-progress-bar`. Das Stylesheet erzeugt das Tailwind-CSS-Standalone-CLI v4.3.3 aus `internal/web/tailwind.css`
  (Tokens der Status-Page als CSS-Variablen, `@theme inline`, Komponentenklassen) und scannt nur `overview.html`
  (`source(none)` + `@source`). Das Binary bettet `app.css` per `embed` ein; der Link traegt die ersten 12 Hex-Stellen
  des SHA-256 (`?v=`), deshalb darf der Browser ein Jahr cachen.
  Die Spalte „Change“ der Umwandlungen zeigt `(OutputSize - SourceSize) / SourceSize * 100` mit einer Nachkommastelle:
  kleiner gruen (`−94.0 %`), groesser rot (`+0.2 %`), gerundet gleich grau (`±0.0 %`), bei Originalgroesse 0 `–`.

Gemessen am 2026-10-29 im Devcontainer, Dienst mit einem Bucket gegen `http://127.0.0.1:1` (nicht erreichbar):

| Anfrage | Ergebnis |
|---|---|
| `GET /healthz` | `200 ok` |
| `GET /` ohne / mit falschem / mit richtigem Basic | `401` mit Challenge / `401` / `200` HTML |
| `GET /api/status` ohne / mit falschem / mit richtigem Bearer | `401` mit Challenge / `401` / `200`, `lastScanError` mit `connection refused` |
| `GET /api/buckets/content/files?limit=5000` | `400` |
| `GET /api/buckets/content/files/aktion/Foto%201.JPG` | `404 unknown file` |
| `GET /api/buckets/nope/files` | `404 unknown bucket` |
| leeres `ui_password` und `api_token` | `/` und `/api/status` `403`, `/healthz` `200` |
| SIGTERM | `shutting down`, `s3-webx stopped`, keine Secrets im Log |

## Image und CI

```
Dockerfile
  css     debian:13.7-slim + Tailwind-CLI v4.3.3 (ADD --checksum sha256:dc61b3ac…)  ──▶ app.css (--minify)
  build   golang:1.26.8-alpine3.24 + vips-tools + ffmpeg
          go vet ──▶ go test -v (Abbruch bei FAIL oder SKIP) ──▶ go build (statisch, -trimpath, -s -w, main.version)
          ──▶ Lizenztexte sammeln (LICENSE, Go, alle gelinkten Module) nach /out/licenses
  runtime alpine:3.24.1 + vips-tools + ffmpeg + ca-certificates + tzdata
          /usr/local/bin/s3-webx · /usr/share/licenses/s3-webx · USER 10001 · VOLUME /data · EXPOSE 8080 · HEALTHCHECK wget /healthz
```

- **Stylesheet:** Der Stage `css` laedt `tailwindcss-linux-x64` per `ADD --checksum` mit der SHA-256 aus dem
  GitHub-Release und erzeugt `app.css`; der Build-Stage kopiert sie nach `internal/web/` vor `go vet` und `go test`.
  Lokal macht `./dev css` dasselbe im Devcontainer (Binary nach `bin/tailwindcss-v4.3.3`, Pruefung per `sha256sum -c`);
  `build`, `test` und `lint` rufen es, wenn `app.css` fehlt oder aelter ist als `tailwind.css` oder `overview.html`.
- **Tests im Build:** `go test` laeuft im Build-Stage mit denselben `vips`- und `ffmpeg`-Paketen, die auch im
  Laufzeit-Image stehen. Die Konverter-Tests ueberspringen sich ohne die Werkzeuge (`t.Skipf`); der Build bricht
  deshalb auch bei einem `--- SKIP` ab, sonst koennte ein fehlendes Paket unbemerkt durchrutschen.
  `config.example.yaml` kommt in den Build-Kontext, weil `TestExampleConfigIsValid` sie liest.
- **Versionen:** Alpine 3.24.1 bringt vips 8.18.2 und FFmpeg 8.1.3 (mit `libvpx-vp9` und `libopus`); der Devcontainer
  (Debian bookworm) hat vips 8.14 und FFmpeg 5.1. Die Tests sind in beiden gruen. `stripWebPMetadata` bleibt im
  Ablauf, gleich ob die vips-Version `strip` selbst umsetzt.
- **Binary:** `CGO_ENABLED=0`, weil SQLite ueber `modernc.org/sqlite` ohne cgo laeuft. `VERSION` setzt
  `main.version`; `./dev image` uebergibt den kurzen Commit, die CI den vollen SHA.
- **Laufzeit:** Benutzer `s3webx` mit UID/GID 10001, `/data` gehoert ihm. `ENTRYPOINT` ist das Binary, `CMD`
  `-config /config/s3-webx.yaml`. Der `HEALTHCHECK` ruft busybox-`wget` gegen `127.0.0.1:8080/healthz` und setzt den
  Default-Port voraus.
- **Groesse:** `baunach/s3-webx:local` belegt 280 MB auf der Platte, 77,7 MB komprimiert. Den Grossteil tragen FFmpeg
  und libvips mit ihren Codec-Bibliotheken.
- **`.dockerignore`:** nur `go.mod`, `go.sum`, `config.example.yaml`, `LICENSE`, `cmd/`, `internal/` gehen in den
  Kontext.
- **CI** (`.github/workflows/ci.yaml`, Vorbild `postgres-backup`) hat zwei Jobs, Entscheidung des Users vom
  2026-10-29: PR-Builds laufen auf Gitea, Release-Builds ausschliesslich auf GitHub.
  - `test` laeuft nur bei `pull_request`: Checkout, Buildx, `docker/build-push-action` mit `push: false` fuer
    `linux/amd64`. Der Build-Stage fuehrt dabei die Tests aus. Kein Login, keine Secrets.
  - `release` laeuft bei Push auf `main`, `workflow_dispatch` und Cron `0 3 * * 0`, aber nur, wenn
    `github.server_url == 'https://github.com'`. Er meldet sich mit `DOCKER_USERNAME`/`DOCKER_ACCESSTOKEN_RW` an und
    pusht `baunach/s3-webx:latest` und `:<voller SHA>` mit `VERSION=<SHA>`.
  - Gitea Actions liest `.github/workflows/` ebenfalls, setzt `github.server_url` aber auf die eigene Adresse
    (`https://gitea.baunach.work`). Ein Push auf `main` in Gitea startet deshalb keinen Release; auf GitHub laeuft
    umgekehrt kein PR-Build, weil dort keine Pull Requests entstehen (nur Mirror).
- **Lizenz:** eigener Code unter MIT-0 (`LICENSE`). Der Build-Stage sammelt nach dem `go build` per `go list -deps`
  die Lizenz-, Notice- und Patent-Dateien aller Module, die ins Binary eingehen, dazu die Go-Lizenz aus `GOROOT` und
  das eigene `LICENSE`, nach `/out/licenses/<modul>@<version>/`; die Laufzeit kopiert das nach
  `/usr/share/licenses/s3-webx/` (484 KB). Ein Modul ohne Lizenzdatei bricht den Build ab. FFmpeg (GPL-2.0-or-later
  AND LGPL-2.1-or-later) und libvips (LGPL-2.1-or-later) kommen als Alpine-Pakete; `s3-webx` startet sie als eigene
  Prozesse und linkt sie nicht. Ihre Lizenzangaben zeigt `apk list -I` im Image, die Quellen liegen in den
  Alpine-Paketquellen (`https://gitlab.alpinelinux.org/alpine/aports`). Die Modul-Lizenzen im Einzelnen: README,
  Abschnitt "License".

## Sandbox-Test

Am 2026-10-29 lief das Image `baunach/s3-webx:local` als Container `s3-webx-sandbox-test` im Netz `bis-network` gegen
das MinIO `baunach-s3-cloudflare` des Shared Static Stack, mit `interval: 1m`. Der Bucket `s3-webx-test` enthielt eine
Kopie aller 43 Dateien aus `content-local/aktion/2026-10-rstorc/` und `loesungen/videos/ruecklaufnutzung.mp4`; die
Content-Buckets selbst blieben unberuehrt. Der Bucket `s3-webx-test` mit den Ergebnissen steht weiter im MinIO.

| Datei | vorher | nachher | Ersparnis | Dauer |
|---|---|---|---|---|
| 14 Praxisfotos `2026-…-HGB.jpg` und `…-retuschiert-AT.jpg` (2–4 MB, Handy, EXIF) | 38.390.287 B | 2.422.368 B | 94 % | 0,3–1,3 s je Datei |
| 28 PNG und JPEG der Aktion (40 KB–1 MB) | 5.354.673 B | 927.274 B | 83 % | 0,1–1,2 s je Datei |
| `Puffer-Be-undEntladung-2020-11-09-JB.gif` (640×360, 333 Einzelbilder) | 2.509.669 B | 2.095.536 B | 17 % | 7,4 s |
| **Bilder gesamt (43)** | **46.254.629 B** | **5.445.178 B** | **88 %** | **36 s** |
| `ruecklaufnutzung.mp4` (H.264 1680×1050, AAC, 16:06 min) | 118.925.713 B | 42.894.641 B | 64 % | 13:49 min |
| **Summe (44)** | **165.180.342 B** | **48.339.819 B** | **71 %** | |

- **Stichprobe Bild:** `2026-06-17_081816-HGB.jpg` (4000×2252, EXIF-Orientation 6, Kamera `samsung`) →
  `….jpg.webp` 901×1600 hochkant, also richtig gedreht und auf 1600 begrenzt, ohne EXIF-Felder. Das GIF ergibt ein
  animiertes WebP mit 333 Einzelbildern, 640×360, Schleife `loop 0`. Transparente PNG behalten den Alphakanal (4 Baender).
- **Stichprobe Video:** `….mp4.webm` ist WebM mit VP9 Profile 0, `yuv420p`, 1680×1050 (kuerzere Kante unter 1080,
  also nicht verkleinert) und Opus 48 kHz Stereo, Dauer gleich dem Original. Die Umwandlung lief mit rund 1,2-facher
  Echtzeit; die HTML-Uebersicht zeigte waehrenddessen Laufzeit und Prozent mit Balken (`running for 31s · 4.4 %`).
- **Content-Type** laut `mc stat`: `image/webp` bzw. `video/webm`. Die Originale tragen im Sandbox-MinIO kein
  `Cache-Control`, die Varianten deshalb auch keines.
- **Zweiter Durchlauf:** Die Scans nach dem Ende meldeten `images 0, videos 0`; auch nach einem Neustart des
  Containers (Zustand im Volume) wurde nichts neu umgewandelt. Der `HEALTHCHECK` meldete danach `healthy`.
- **Neue Uebersicht:** Nach dem Redesign lief das Image als `s3-webx-ui-check` gegen denselben Bucket, nachdem dessen
  Varianten entfernt waren. Headless Chromium (Playwright) bei 390 und 1280 px, hell und dunkel: keine CSP-Meldung in
  der Konsole, `scrollWidth` gleich der Fensterbreite, Bild wie der Entwurf. Ein hochgeladenes 0-Byte-JPEG erschien
  sofort unter „Failures“ mit `source object is empty (0 bytes)` und 3 Versuchen.
- **Folgerung fuer die Webseite:** Die Leiste „In 10 Schritten montiert“ schrumpft von rund 38 MB auf 2,4 MB. Das GIF
  bringt als WebP wenig; ein Video (WebM/MP4 als `<video>`) waere fuer diese Animation die bessere Form.

## Selbst getroffene Entscheidungen

| Entscheidung | Wahl | Grund |
|---|---|---|
| Reihenfolge „aelteste zuerst“ | nach `queued_at`, bei Gleichstand LastModified des Originals | Wer am laengsten wartet, kommt zuerst dran; kein Bucket verhungert |
| Bekannte Formate | Bilder `jpg jpeg png gif tif tiff`, Videos `mp4 mov m4v mkv avi` | was vips bzw. ffmpeg im Image sicher lesen; alles andere ist ein Konfigurationsfehler |
| Zugangsdaten | `access_key` und `secret_key` Pflicht | ohne Schreibrecht kann der Dienst nichts ablegen |
| Endpoint ohne Schema | `https` | sicherer Default; MinIO im lokalen Netz traegt `http://` ausdruecklich |
| `${NAME}`-Ersetzung | im YAML-Baum, nicht im Rohtext | Secrets mit YAML-Sonderzeichen bleiben unverfaelscht |
| EXIF bei WebP | Chunks selbst entfernen, ICC behalten | vips 8.14 ignoriert `strip` bei `webpsave` |
| Video-Geschwindigkeit | `-deadline good -cpu-used 2` | Default ist viel langsamer bei kaum sichtbarem Gewinn |
| Video-Pixelformat | `yuv420p` erzwingen | breite Abspielbarkeit im Browser |
| Neue Quellversion waehrend der Umwandlung | Ergebnis nur bei passender Version verbuchen | sonst bliebe die neue Fassung ohne Variante |
| Abbruch beim Shutdown | `queued`, kein Versuch gezaehlt | der Fehler lag nicht an der Datei |
| Aufraeumen beim Start | nur `job-*` im Arbeitsordner | ein falsch gesetzter `state`-Pfad loescht so keine fremden Dateien |
| Zustand im Test | echtes SQLite statt Double | weniger Code, und die Abfragen selbst werden mitgeprueft |
| Log-Format | JSON auf stdout | maschinenlesbar im Container-Log |
| Arbeiter im Leerlauf (API) | `{"kind": …, "idle": true}` statt `null` | jeder Eintrag traegt seine Art, der Client muss nicht nach Position raten |
| `percent` | nur beim Video-Arbeiter | der Bild-Arbeiter meldet keinen Fortschritt |
| `counts` | immer alle fuenf Status je Art, auch mit 0 | feste Form fuer Webseite und Backend |
| Letzter Scan | im Speicher, mit Fehlertext `lastScanError` | Scan laeuft beim Start sofort; ein nicht erreichbarer Bucket wird so sichtbar |
| Bytes | nur `done` | nur deren Variante liegt im Bucket |
| `limit` ausserhalb 1–1000 | `400`, nicht stillschweigend gekappt | Vorgabe „ungueltige Parameter → 400“; der Client merkt den Fehler |
| `status`-Filter | wiederholt oder kommagetrennt, mehrere Werte | die Uebersicht braucht `done` + `skipped` zusammen, die API bekommt es gleich mit |
| Feldname Variantengroesse | `variantSize` | passt zu `variantKey`; bei `skipped` die gemessene, nicht hochgeladene Groesse |
| Laenge beim Vergleich | SHA-256 vor `ConstantTimeCompare` | sonst verraet die Laufzeit die Laenge des Secrets |
| Unbekannte Pfade unter `/api/` | erst Token, dann `404` | ohne Token laesst sich die API nicht abtasten |
| Port binden | vor Scannern und Arbeitern | belegter Port beendet den Start, statt halb zu laufen |
| Kopfzeilen der Uebersicht | CSP ohne Skripte, `no-store`, `nosniff`, `no-referrer` | Seite enthaelt Schluessel aus fremden Buckets |
| `app.css` im Repo | nicht committet (`.gitignore`), erzeugt von `./dev` und Dockerfile | kein generierter Diff im Review; das Image baut sie immer frisch, also kann keine veraltete Datei ausgeliefert werden |
| Tailwind-Binary | glibc-`linux-x64` in einem Debian-Stage statt `-musl` im Alpine-Stage | ein Binary und eine Pruefsumme fuer Dockerfile und Devcontainer (Debian bookworm) |
| Schutz von `/assets/app.css` | Basic wie `/` | der Browser schickt die Zugangsdaten fuer denselben Host ohnehin mit; keine offene Flaeche ausser `/healthz` |
| Cache des Stylesheets | `private, max-age=31536000, immutable` mit Hash im Link | Antwort auf eine authentifizierte Anfrage gehoert in keinen geteilten Cache |
| Balken ohne Inline-Stil | `<progress>` | semantisch und barrierearm, haelt die CSP ohne `unsafe-inline`; Breitenklassen in 1-%-Schritten waeren 101 Klassen |
| Seitenzahl ausserhalb | Seite 1 bzw. letzte Seite | Links aus einer aelteren Ansicht fuehren nie ins Leere |
| Leeres Original | sofort `failed` mit `MaxAttempts` | Download und Konverter koennen bei 0 Byte nichts liefern; drei gleiche Fehler waeren Rauschen |
| Laufzeit-Basis | `alpine:3.24.1` mit Distributionspaketen | vips und FFmpeg mit VP9/Opus fertig gebaut; kleiner als Debian, ohne eigenen Codec-Build |
| Uebersprungene Tests im Build | Abbruch | ein fehlendes `vips` oder `ffmpeg` im Build-Stage faellt sonst nicht auf |
| Benutzer | UID/GID 10001, ohne Login-Shell und Home | fester Wert fuer Bind-Mounts, keine Kollision mit System-Benutzern |
| `HEALTHCHECK` | busybox-`wget` auf Port 8080 | im Image vorhanden, kein eigenes Flag noetig; abweichender `listen` braucht eigenen Healthcheck |
| SHA-Tag | voller Commit-SHA | eindeutig und ohne eigenen Schritt aus `github.sha` |
| PR-Lauf in der CI | bauen und testen ohne Login und Push | kommt ohne Secrets aus; ein Fork-PR kann nichts veroeffentlichen |
