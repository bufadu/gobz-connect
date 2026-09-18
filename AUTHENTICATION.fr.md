# Authentification

🇬🇧 [Read this document in English](AUTHENTICATION.md)

## Contexte

Depuis avril 2026, Qobuz a ajouté un reCAPTCHA sur son endpoint API
`user/login`, ce qui casse la connexion directe par email/mot de passe pour
tous les outils tiers. L'authentification par token est désormais la seule
méthode supportée.

## Comment obtenir tes identifiants

1. Ouvre [https://play.qobuz.com/login](https://play.qobuz.com/login) dans un navigateur et connecte-toi avec ton compte Qobuz.
2. Ouvre les DevTools (F12 ou Ctrl+Shift+I / Cmd+Option+I sur macOS).
3. Va dans l'onglet **Network**.
4. Tape `user/login` dans la barre de filtre.
5. Recharge la page et attends que Qobuz ait fini de charger.
6. Clique sur la requête `login` qui apparaît dans la liste.
7. Ouvre l'onglet **Response** dans le panneau de droite.
8. Copie deux valeurs depuis la réponse JSON :
   - `"id"` → c'est ton **user ID** (une chaîne numérique)
   - `"user_auth_token"` → c'est ton **token d'authentification** (une longue chaîne alphanumérique)

## config.yaml

```yaml
user_id: "12345678"
user_auth_token: "your-user-auth-token-here"
```

Les champs `email` et `password` ne sont plus nécessaires et peuvent être
supprimés ou laissés vides.

## Durée de vie du token

`user_auth_token` est un token de session longue durée — il n'expire pas
après une durée fixe comme un JWT. Il reste valide jusqu'à ce que tu te
déconnectes explicitement de Qobuz ou que tu révoques les sessions depuis les
paramètres de ton compte. Tu ne devrais pas avoir besoin de le renouveler
souvent.

## Mode non-authentifié (expérimental)

Si tu préfères ne mettre aucun identifiant Qobuz dans `config.yaml`,
gobz-connect peut se passer entièrement d'authentification locale :

```yaml
unauthenticated_mode: true
```

Avec ce réglage, `user_id`/`user_auth_token` ne sont pas nécessaires. À la
place, gobz-connect ne démarre que son annonce mDNS et son écouteur
WebSocket, et attend que l'application Qobuz le sélectionne dans le
sélecteur d'appareils Connect. Quand l'application se connecte, elle
transmet au renderer ses propres identifiants éphémères (`jwt_api`) via
mDNS (`connect-to-qconnect`) — c'est la même remise de jeton que
l'application utilise pour contrôler n'importe quel appareil Connect, rien
de spécifique à gobz-connect.

**Le piège :** cette remise de jeton n'a lieu que quand un utilisateur
sélectionne activement ce renderer dans l'interface de l'application.
`jwt_api` expire au bout d'un moment, et rien dans le protocole de
l'application Qobuz ne garantit qu'elle le renouvellera spontanément — seule
une re-sélection du renderer le fait. En pratique, ça veut dire :

- Les morceaux déjà téléchargés/en file d'attente continuent de jouer
  normalement même après l'expiration du token.
- Charger un *nouveau* morceau (un qui n'a pas déjà été récupéré) peut
  échouer une fois le token expiré, jusqu'à ce que l'application se
  reconnecte — gobz-connect attend jusqu'à 45 secondes que ça arrive avant
  d'abandonner la requête et de passer à la suite.
- Pour une session d'écoute longue et sans surveillance (ex. de la musique
  de fond qui joue pendant des heures sans que personne ne touche
  l'application Qobuz), il est possible que la lecture finisse par ne plus
  avancer dans la file d'attente, jusqu'à ce que tu rouvres l'application et
  resélectionnes le renderer.

C'est une limitation du protocole Connect sous-jacent, pas quelque chose que
gobz-connect peut entièrement contourner de lui-même. Pour un usage longue
durée sans surveillance, préfère `user_id`/`user_auth_token` ci-dessus, qui
se renouvelle tout seul et ne dépend pas d'une reconnexion de l'application.
Réserve `unauthenticated_mode` aux tests rapides quand tu ne veux pas du
tout configurer d'identifiants.
