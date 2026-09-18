# gobz-connect

🇬🇧 [Read this document in English](README.md)

Un renderer Qobuz Connect écrit en Go, conçu en priorité pour tourner sur un
**Raspberry Pi** — diffusion audio en HDMI directement vers un amplificateur
ou un ampli-tuner, avec un contrôle HDMI-CEC qui allume/éteint l'ampli et
bascule son entrée automatiquement. Il fonctionne aussi très bien sur
**macOS** pour un usage quotidien — une bonne façon de recycler un vieux Mac
mini en point de diffusion Qobuz Connect dédié, par exemple. Le renderer
s'annonce via mDNS pour que l'application mobile Qobuz puisse le découvrir et
lui envoyer de l'audio, exactement comme un appareil Chromecast ou Sonos.

## Fonctionnalités

- **Pensé pour Raspberry Pi en priorité** : conçu pour tourner sans écran sur
  un Pi, avec l'ampli/ampli-tuner branché directement en HDMI — pas besoin de
  DAC ou de carte son séparée.
- **Lecture sans coupure (gapless)** entre les morceaux qui partagent la même
  fréquence d'échantillonnage — le cas le plus courant pour la plupart des
  albums et playlists.
- **Support de tous les formats audio jusqu'au Hi-Res 192 kHz/24-bit, même
  sur un Raspberry Pi** — le moteur de fréquence d'échantillonnage adaptative
  réinitialise le périphérique ALSA à la fréquence native de chaque morceau
  au lieu de le ré-échantillonner, ce qui garde une charge CPU assez basse
  pour qu'un Pi gère le Hi-Res sans à-coups. Voir
  [Comportement de la sortie audio](#comportement-de-la-sortie-audio) plus bas.
- Fonctionne aussi très bien sur **macOS** (CoreAudio) pour un usage
  quotidien.
- **HDMI-CEC** *(expérimental)* : allume automatiquement l'ampli au démarrage
  de la lecture, bascule son entrée sur le Pi, et l'envoie en veille après
  une période d'inactivité. Les implémentations CEC varient beaucoup selon
  les marques et modèles d'amplis — ça peut ne pas fonctionner avec tous les
  amplis. Voir [Raspberry Pi — configuration HDMI & CEC](#raspberry-pi--configuration-hdmi--cec)
  plus bas.

---

## Installation sur Raspberry Pi

`install.sh` automatise toute la mise en place : dépendances système,
toolchain Go, compilation, utilisateur de service dédié, et service systemd.
À lancer directement sur le Pi.

### 1. Installer git et cloner le dépôt

```bash
sudo apt update
sudo apt install -y git
git clone https://github.com/bufadu/gobz-connect.git
cd gobz-connect
```

### 2. Lancer l'installateur

```bash
sudo bash install.sh
# ou, pour le support HDMI-CEC (expérimental — voir la section HDMI & CEC plus bas) :
sudo bash install.sh --cec
```

Ça installe les dépendances système (`build-essential`, `pkg-config`,
`libasound2-dev`, plus les bibliothèques CEC si `--cec` est utilisé), installe
Go s'il est absent ou trop ancien, compile le binaire, l'installe dans
`/usr/local/bin/gobz-connect`, crée un utilisateur système dédié
`gobz-connect`, et installe — sans le démarrer — un service systemd.

Relancer `install.sh` plus tard effectue une mise à jour sur place : il
recompile depuis la dernière version du code source et redémarre le service
pour toi.

### 3. Modifier la configuration

```bash
sudo nano /etc/gobz-connect/config.yaml
```

Un exemple commenté y est écrit lors de la première installation. Renseigne
au minimum `user_id` et `user_auth_token` (voir
[AUTHENTICATION.fr.md](AUTHENTICATION.fr.md)), ou active
`unauthenticated_mode: true`. Voir la [référence complète](#référence-complète)
plus bas pour tous les réglages disponibles.

### 4. Démarrer le service

```bash
sudo systemctl start gobz-connect
```

`install.sh` a déjà activé le démarrage automatique du service au boot.

### 5. Commandes utiles

```bash
# Suivre les logs en direct
sudo journalctl -u gobz-connect -f

# Redémarrer après avoir modifié la config
sudo systemctl restart gobz-connect

# Vérifier si le service tourne
sudo systemctl status gobz-connect

# Arrêter le service
sudo systemctl stop gobz-connect
```

### Désinstallation

```bash
sudo bash uninstall.sh          # conserve la config et le cache
sudo bash uninstall.sh --purge  # supprime aussi la config, les secrets et le cache
```

---

## Configuration

Tous les réglages vivent dans un unique fichier `config.yaml`.

### Authentification

gobz-connect a besoin d'identifiants Qobuz pour récupérer les métadonnées des
morceaux et les URLs de streaming. Voir **[AUTHENTICATION.fr.md](AUTHENTICATION.fr.md)**
pour savoir comment obtenir un token, et pour la liste complète des modes
d'authentification supportés, y compris `unauthenticated_mode`, où les
identifiants sont fournis par l'application Qobuz elle-même plutôt que
configurés localement.

### Exemple minimal

```yaml
# Authentification par token — voir AUTHENTICATION.fr.md pour savoir comment les obtenir
user_id: "123456789"
user_auth_token: "your_auth_token_here"

device_name: Mon Renderer
port: 1984
```

### Référence complète

```yaml
# ── Authentification ────────────────────────────────────────────────────────
# Voir AUTHENTICATION.fr.md pour savoir comment obtenir ces valeurs.
user_id: ""             # User ID depuis le lecteur web Qobuz
user_auth_token: ""     # Token d'authentification depuis le lecteur web Qobuz

unauthenticated_mode: false
# Expérimental. Alternative à user_id/user_auth_token : ignorer toute
# authentification locale et se reposer entièrement sur l'application Qobuz
# pour fournir les identifiants via mDNS (connect-to-qconnect) quand elle se
# connecte à ce renderer. Ces identifiants sont éphémères et seule
# l'application peut les renouveler — rien ne garantit qu'elle le fera
# automatiquement, donc une session longue sans surveillance peut finir par
# perdre la capacité de charger de nouveaux morceaux jusqu'à ce que
# l'application se reconnecte.
# Préfère user_id/user_auth_token ci-dessus pour un usage longue durée sans
# surveillance.

# ── Appareil ─────────────────────────────────────────────────────────────────
device_name: QobuzConnect   # Nom affiché dans la liste des renderers de l'app Qobuz
port: 1984                  # Port HTTP pour les endpoints de découverte mDNS

# ── Qualité audio ────────────────────────────────────────────────────────────
audio_format: 6   # Format de flux Qobuz demandé
                  #   5  = MP3
                  #   6  = FLAC CD (16-bit 44.1 kHz)  ← par défaut
                  #   7  = Hi-Res 96 kHz
                  #  27  = Hi-Res 192 kHz

# ── Sortie audio ─────────────────────────────────────────────────────────────
speaker_sample_rate: 0
# Fréquence d'échantillonnage de sortie en Hz (ex. 44100, 48000, 96000, 192000).
# 0 (par défaut) : détection automatique de la fréquence maximale supportée
# par le périphérique ALSA/CoreAudio par défaut.
#
# Avec adaptive_sample_rate activé (par défaut sous Linux), ce réglage agit
# comme un plafond matériel : la sortie est réinitialisée à chaque morceau à
# sa fréquence native, mais jamais au-dessus de cette valeur. Utile quand le
# périphérique audio ne supporte physiquement pas certaines fréquences
# (ex. un DAC USB limité à 96 kHz).
#
# Avec adaptive_sample_rate désactivé, ce réglage fixe la sortie à une seule
# fréquence et tous les morceaux qui en diffèrent sont ré-échantillonnés.

adaptive_sample_rate: true
# Linux uniquement. Quand true (par défaut), gobz-connect réinitialise le
# périphérique audio ALSA à la fréquence native de chaque morceau. Bénéfices :
#   • Aucun ré-échantillonnage dans le cas courant → zéro surcharge CPU sur
#     un Raspberry Pi.
#   • Chaque morceau joue à sa vraie qualité (les CD 44.1 kHz en 44.1 kHz,
#     le Hi-Res 96 kHz en 96 kHz, etc.).
#
# Contrepartie : quand deux morceaux consécutifs ont des fréquences
# différentes, le périphérique est réinitialisé entre les deux, produisant un
# bref silence (~100–200 ms) au lieu d'une transition sans coupure. Au sein
# d'une playlist à fréquence unique (tout en CD ou tout en Hi-Res), la
# lecture reste entièrement gapless.
#
# Sous macOS, adaptive_sample_rate n'a aucun effet : le contexte audio de
# CoreAudio ne peut pas être fermé puis rouvert, donc le périphérique reste
# toujours verrouillé sur une fréquence fixe (détectée automatiquement ou
# speaker_sample_rate) et les morceaux sont ré-échantillonnés au besoin. Le
# ré-échantillonnage sous macOS utilise le SRC matériel de CoreAudio et est
# essentiellement gratuit ; aucun à-coup audio ne se produit.
#
# Mets à false pour verrouiller le périphérique sur une fréquence fixe et
# ré-échantillonner tous les morceaux, ce qui peut causer des à-coups audio
# sur un Raspberry Pi en cas de suréchantillonnage (ex. 44.1 → 96 kHz).

resample_quality: 4
# Qualité du ré-échantillonneur quand la fréquence d'un morceau doit être
# convertie. Avec adaptive_sample_rate activé, le ré-échantillonneur n'est
# sollicité que quand speaker_sample_rate plafonne la fréquence native d'un
# morceau (ex. un morceau à 192 kHz plafonné à 96 kHz), donc ce réglage
# compte rarement dans ce mode. Avec adaptive_sample_rate désactivé, c'est
# critique sur les appareils peu puissants.
#
# valeur | compromis
# -------|-----------
#  1     | le plus rapide, qualité la plus basse — pour du matériel très lent
#  4     | bon équilibre (par défaut)
#  6     | meilleure qualité, plus de CPU — à la limite pour du temps réel
# >6     | usage hors-ligne/archivage uniquement ; trop lent pour du temps réel

# ── Cache des pistes ─────────────────────────────────────────────────────────
cache_dir: /tmp/qobuz-cache   # Répertoire du cache local des morceaux
cache_size_mb: 1024           # Taille maximale du cache en Mo, 0 pour désactiver

background_download_rate_kbps: 0
# Limite le débit (en Ko/s) des téléchargements en arrière-plan (le morceau
# suivant, récupéré à l'avance) pour réduire la contention d'I/O sur la carte
# SD avec le morceau en cours de lecture. Le téléchargement du morceau en
# cours de lecture n'est jamais limité. 0 (par défaut) = illimité.

# ── Secrets ──────────────────────────────────────────────────────────────────
secrets_file: qobuz-secrets.json
# Chemin du fichier où gobz-connect stocke les identifiants d'application
# Qobuz récupérés par scraping (AppID et AppSecret). Généré automatiquement
# au premier lancement.

# ── HDMI-CEC ─────────────────────────────────────────────────────────────────
cec:
  enable: false
  standby_delay: 15     # minutes d'inactivité avant d'envoyer l'ampli en veille
  volume_control: false # route le curseur de volume Qobuz vers l'ampli via CEC
  # Avancé, utile seulement si la découverte automatique de l'ampli échoue
  # sur ton adaptateur (fréquent avec l'adaptateur CEC intégré VC4 du
  # Raspberry Pi) — voir la section configuration HDMI & CEC plus bas.
  fallback_log_addr: 0    # 0 = non défini ; utilise la découverte automatique
  fallback_phys_addr: ""  # ex. "3.0.0.0" ; vide = non défini
# Nécessite un binaire compilé avec -tags cec. Voir BUILD.md pour les étapes
# de compilation et la section configuration HDMI & CEC plus bas pour la
# configuration système à faire une seule fois.
```

---

## Comportement de la sortie audio

### Vue d'ensemble par plateforme

gobz-connect utilise différents moteurs audio selon la plateforme :

| Plateforme | Moteur audio | SR adaptative | Ré-échantillonnage si la SR diffère |
|----------|--------------|-------------|---------------------------|
| **Linux** | ALSA maison (CGo) | Oui — le PCM est rouvert à chaque morceau | Seulement quand `speaker_sample_rate` plafonne la fréquence native du morceau |
| **macOS** | CoreAudio via beep/speaker + oto | Non — le contexte oto est permanent | Toujours (gratuit : SRC matériel de CoreAudio) |

### Linux — fréquence d'échantillonnage adaptative (par défaut)

Sous Linux, gobz-connect utilise un moteur ALSA maison capable de fermer et
rouvrir le périphérique PCM à n'importe quelle fréquence entre deux morceaux.

**Premier morceau :** l'initialisation de la sortie est différée jusqu'à ce
que le premier morceau soit connu, donc le périphérique s'ouvre toujours à la
fréquence native de ce morceau — aucun ré-échantillonnage ne se produit, même
sur le tout premier morceau.

**Playlist à fréquence unique** (ex. tout en CD 44.1 kHz ou tout en Hi-Res
96 kHz) : le périphérique est ouvert une seule fois et la lecture est
entièrement gapless, sans surcharge CPU.

**Playlist à fréquences mixtes** (ex. un morceau CD suivi d'un morceau
Hi-Res) : gobz-connect attend la fin du morceau en cours, vide le tampon PCM,
ferme le périphérique, puis le rouvre à la nouvelle fréquence. Ça produit un
bref silence (~100–200 ms) à la frontière de fréquence au lieu d'une
transition sans coupure. Aucun ré-échantillonnage n'est effectué.

| Morceau | SR native | Sortie initialisée à | Ré-échantillonnage |
|-------|-----------|-------------------|------------|
| CD FLAC | 44.1 kHz | 44.1 kHz | aucun |
| Hi-Res | 96 kHz | 96 kHz | aucun |
| Hi-Res (plafonné) | 192 kHz | 96 kHz (`speaker_sample_rate: 96000`) | 192 → 96 kHz |

### Linux — fréquence d'échantillonnage fixe (`adaptive_sample_rate: false`)

Le périphérique est initialisé une seule fois au démarrage (détecté
automatiquement ou via `speaker_sample_rate`) et tous les morceaux sont
ré-échantillonnés à cette fréquence. Le suréchantillonnage (ex. 44.1 →
96 kHz) est coûteux en CPU et peut causer des à-coups audio sur un Raspberry
Pi. Utilise `resample_quality: 1` pour réduire la charge CPU au prix de la
qualité, ou laisse `adaptive_sample_rate` à sa valeur par défaut.

### macOS

Le contexte audio oto est créé une seule fois et ne peut pas être recréé. Le
périphérique reste verrouillé sur la fréquence détectée au démarrage (ou
`speaker_sample_rate`). Tous les morceaux dont la fréquence native diffère
sont ré-échantillonnés par beep, à la qualité définie par `resample_quality`.
CoreAudio effectue la conversion de fréquence efficacement en matériel, donc
le ré-échantillonnage n'est pas un problème de CPU sous macOS.
`adaptive_sample_rate` n'a aucun effet sous macOS.

---

## Réglages Raspberry Pi

### Alimentation

Les à-coups audio et les blocages lors des recherches sont souvent causés par
un throttling CPU dû à une sous-tension. Vérifie `dmesg | grep -i volt` et
utilise une alimentation officielle 5V/3A. Une mauvaise alimentation causera
des problèmes quels que soient les réglages logiciels.

### Carte SD

gobz-connect met en cache les morceaux téléchargés sur le disque en continu
pendant la lecture, ce qui veut dire des lectures/écritures petites et
mélangées, sur la même carte qui héberge aussi l'OS — pas les gros transferts
séquentiels autour desquels la plupart des classes de vitesse sont pensées.

- Privilégie une carte notée **A1 ou A2** (Application Performance Class).
  Ces classes garantissent un minimum d'IOPS aléatoires, ce qui est
  exactement ce que ce type de charge sollicite — les chiffres de Mo/s
  séquentiels indiqués sur l'emballage ne disent pas grand-chose ici.
- **U3 ou V30** (débit d'écriture séquentiel minimum soutenu) est un bon
  indicateur secondaire d'une carte avec un contrôleur correct.
- Évite les cartes bas de gamme ou sans marque. Des performances
  incohérentes sous charge mixte lecture/écriture sont une cause fréquente
  de saccades audibles pendant les téléchargements Hi-Res. Si ça arrive avec
  une carte qui respecte pourtant les classes ci-dessus, essaie de réduire
  `background_download_rate_kbps` (voir la référence de configuration) pour
  diminuer la pression en écriture pendant le préchargement d'un morceau en
  arrière-plan.

### Raspberry Pi — configuration HDMI & CEC

Cette section est spécifique à Raspberry Pi OS (Raspbian). C'est une
configuration à faire une seule fois, sur le système d'exploitation du Pi
lui-même, distincte de la compilation de gobz-connect avec le support CEC —
voir [BUILD.md](BUILD.md) pour ça. À suivre si tu veux de l'audio en HDMI et,
optionnellement, un contrôle HDMI-CEC de ton ampli.

**Le support CEC est expérimental.** HDMI-CEC est connu pour son
incohérence entre fabricants — un comportement qui fonctionne bien sur un
ampli peut ne pas fonctionner du tout sur un autre, même quand les deux
revendiquent la conformité CEC. Traite ça comme un bonus, pas une garantie,
et garde toujours un moyen d'allumer l'ampli manuellement.

**1. Faire de la sortie HDMI la sortie audio par défaut**

```bash
sudo raspi-config
# System Options → Audio → sélectionner la sortie HDMI
```

Sur un Pi 4/5 avec deux ports HDMI, choisis le port sur lequel ton ampli est
réellement branché. Redémarre, puis vérifie que le périphérique est visible :

```bash
aplay -l
# on attend une carte nommée quelque chose comme "vc4-hdmi" dans la liste
```

**2. Garder le HDMI actif même si l'ampli est lent à signaler le hotplug**

Certains amplis/récepteurs n'affirment le signal de hotplug HDMI qu'une fois
déjà allumés, ce qui peut empêcher le Pi de trouver le périphérique audio (et
CEC) au démarrage. Force-le dans `/boot/firmware/config.txt`
(`/boot/config.txt` sur les versions plus anciennes de Raspberry Pi OS) :

```ini
hdmi_force_hotplug=1
```

**3. Confirmer que le pilote KMS est actif (requis pour le CEC)**

Les images actuelles de Raspberry Pi OS ont déjà ce réglage par défaut, mais
ça vaut la peine de le confirmer — le HDMI-CEC sur le Pi est exposé via le
pilote d'affichage VC4 KMS :

```ini
dtoverlay=vc4-kms-v3d
```

Ne mets **pas** `hdmi_ignore_cec_init=1` dans `config.txt` — ça désactive le
CEC entièrement.

**4. Autoriser l'accès au périphérique CEC**

gobz-connect communique avec `/dev/cec0`, qui appartient au groupe `video`
sur Raspberry Pi OS. Si gobz-connect tourne en tant qu'utilisateur de service
dédié (comme mis en place par `install.sh`), ajoute cet utilisateur au
groupe :

```bash
sudo usermod -aG video gobz-connect
```

**5. Vérifier le CEC indépendamment de gobz-connect**

Avant de déboguer gobz-connect lui-même, vérifie que le Pi peut voir ton
ampli en CEC :

```bash
sudo apt install cec-utils
echo 'scan' | cec-client -s -d 1
```

Ton amplificateur devrait apparaître comme un appareil sur le bus. Si ce
n'est pas le cas, le problème vient du câblage HDMI/CEC ou du réglage CEC de
l'ampli lui-même (souvent appelé "Anynet+", "Bravia Sync", "SimpLink", etc.
selon la marque) — pas de gobz-connect.

Une fois que tout ça est vérifié, compile avec `-tags cec` et active
`cec.enable: true` dans `config.yaml` — voir
[BUILD.md](BUILD.md#cec-configuration) pour les étapes de compilation et la
liste complète des options `cec:`.

---

## Licence et avertissement

Distribué sous [licence MIT](LICENSE).

Ce projet a été écrit avec l'aide de Claude d'Anthropic, en s'appuyant sur
les connaissances du protocole et les approches d'autres implémentations
open-source de Qobuz Connect déjà disponibles sur GitHub. C'est un projet
indépendant et non-officiel, en aucune façon affilié à, approuvé ou
soutenu par Qobuz. « Qobuz » est mentionné uniquement pour décrire
l'interopérabilité avec son service.

---

## Pour aller plus loin

- [AUTHENTICATION.fr.md](AUTHENTICATION.fr.md) — les modes d'authentification en détail
- [BUILD.md](BUILD.md) — comment compiler, cross-compiler, et activer le support CEC
- [PROTOCOL.md](PROTOCOL.md) — notes sur le protocole WebSocket QConnect
