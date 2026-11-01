# Trusting the dashboard certificate

The Controller serves the dashboard (`https://<controller>:8090`) over HTTPS with a certificate signed by the cluster's own CA. Browsers don't know that CA, so the first visit shows a certificate warning. Trust the CA once on each admin computer and the warning goes away.

## Why this is safe

The cluster CA has X.509 **name constraints**: it can only sign certificates for

- DNS names under `.local`, `localhost`, `.home.arpa`, `.internal`, `.lan`
- IP addresses in `127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `fc00::/7`, `::1`

Browsers (Chrome, Edge, Firefox, Safari) enforce these constraints, so even if the Controller's CA key were stolen it could not be used to impersonate an internet site to a computer that trusts it.

## 1. Get the CA certificate

Any of these work:

- Dashboard sign-in page → **Seeing a certificate warning?** → **Download cluster CA** (click through the browser warning once to get there).
- `https://<controller>:8090/ca.crt`
- On the Controller: `/var/lib/aicluster/controller/ca/ca.crt`

Check its SHA-256 fingerprint against the one on the Controller's console (also shown on the sign-in page and by `aicluster cluster`):

```bash
openssl x509 -in aicluster-ca.crt -noout -fingerprint -sha256
```

## 2. Trust it

**Windows** (Chrome, Edge, and the Claude desktop app) — current user only, no admin rights needed:

```bash
certutil -user -addstore Root aicluster-ca.crt
```

Windows shows a security prompt; check the thumbprint and choose **Yes**. To remove it later: `certutil -user -delstore Root "aicluster CA <cluster-id>"`.

**macOS** (Safari, Chrome):

```bash
security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db aicluster-ca.crt
```

**Linux** (Chrome/Chromium use the NSS database):

```bash
certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n aicluster -i aicluster-ca.crt
```

For system tools such as `curl`: `sudo cp aicluster-ca.crt /usr/local/share/ca-certificates/ && sudo update-ca-certificates`.

**Firefox** (all platforms): Settings → Privacy & Security → Certificates → View Certificates → Authorities → Import → tick *Trust this CA to identify websites*.

Restart the browser afterwards.

## 3. Open the dashboard by a name the certificate covers

The Controller's certificate lists `localhost`, `<hostname>.local`, and each of the Controller's private IP addresses. Use one of those, e.g. `https://192.168.1.10:8090` or `https://aic-controller.local:8090`. If the Controller gets a new IP address it reissues its certificate on the next restart.

## Reinstalling the Controller

A reinstalled Controller without a restored backup creates a **new** CA. Remove the old one from your browsers and trust the new one.
