### Install Rune on EC2 (Ubuntu or Amazon Linux)

This guide installs Rune Server (`runed`) as a systemd service on a single EC2 instance. It uses Docker as the runner and BadgerDB for local state.

Prerequisites:
- An AWS EC2 instance (Ubuntu 22.04+ or Amazon Linux)
- SSH access with sudo privileges
- Internet egress to fetch dependencies

Quick install (recommended):
```bash
curl -fsSL https://raw.githubusercontent.com/runestack/rune/master/scripts/install.sh | sudo bash -s -- --version v0.1.0
```

Notes:
- The installer will install Docker (Ubuntu or Amazon Linux), create the `rune` user, install `rune`/`runed`, write `/etc/rune/rune.yaml` and a KEK, install and enable the systemd service.
- To build from source instead of downloading a release: append `--from-source` instead of `--version ...`.
- To customize ports or config path: `--grpc-port`, `--http-port`, `--config /path/to/rune.yaml`.

Verify after install:
```bash
curl -s http://localhost:7861/health || true
systemctl status runed --no-pager || true
rune version || true
```

### Client access (create a token and configure a client)

After the server is up, create a token on the EC2 instance and configure your local CLI to use it.

On the server (as an admin with a working context):
```bash
sudo rune token create \
  --name initial-client \
  --subject-id initial-client \
  --subject-type user \
  --role readwrite \
  --ttl 720h \
  --out-file /tmp/initial-client.token
```

Securely copy the token to your client machine (replace IP and path):
```bash
scp ubuntu@<SERVER_PUBLIC_IP>:/tmp/initial-client.token ./initial-client.token
```

On the client machine (with the Rune CLI installed):
```bash
rune login \
  --server <SERVER_PUBLIC_IP:7863> \
  --token-file ./initial-client.token

# Test
rune status
```

To revoke or rotate a token later:
```bash
rune token revoke --namespace system --name initial-client
```

Manual installation (alternative)

Artifacts used from this repo if doing manual steps:
- `examples/config/runed.service`
- `examples/config/rune.yaml`

Step 1 — Install Docker

Ubuntu:
```bash
sudo apt-get update -y
sudo apt-get install -y ca-certificates curl gnupg lsb-release
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /usr/share/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $UBUNTU_CODENAME) stable" | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update -y
sudo apt-get install -y docker-ce docker-ce-cli containerd.io
sudo systemctl enable --now docker
```

Amazon Linux (AL2023):
```bash
sudo dnf update -y
sudo dnf install -y docker
sudo systemctl enable --now docker
sudo usermod -aG docker $USER
```

Step 2 — Create rune user and directories
```bash
sudo useradd --system --home /var/lib/rune --shell /usr/sbin/nologin rune || true
sudo mkdir -p /etc/rune /var/lib/rune
sudo chown -R rune:rune /var/lib/rune
```

Step 3 — Install binaries

Option A: Download release binaries (recommended once releases are available):
```bash
RUNE_VERSION="v0.1.0" # replace with the desired tag
curl -L -o /tmp/rune.tgz "https://github.com/runestack/rune/releases/download/${RUNE_VERSION}/rune_linux_amd64.tar.gz"
sudo tar -C /usr/local/bin -xzf /tmp/rune.tgz rune runed
```

Option B: Build from source (fallback):
```bash
GO_VERSION=1.22.5
curl -LO https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go${GO_VERSION}.linux-amd64.tar.gz
export PATH=/usr/local/go/bin:$PATH
git clone https://github.com/runestack/rune.git
cd rune
make build
sudo install -m 0755 bin/rune /usr/local/bin/rune
sudo install -m 0755 bin/runed /usr/local/bin/runed
```

Step 4 — Configure Rune
```bash
sudo cp examples/config/rune.yaml /etc/rune/rune.yaml
sudo sed -i 's#data_dir: "/var/lib/rune"#data_dir: "/var/lib/rune"#' /etc/rune/rune.yaml
```

Generate a 32-byte base64 KEK for secret encryption:
```bash
openssl rand -base64 32 | sudo tee /etc/rune/kek.b64 >/dev/null
sudo chmod 600 /etc/rune/kek.b64
sudo chown root:root /etc/rune/kek.b64
```

Step 5 — Install and enable systemd service
```bash
sudo cp examples/config/runed.service /etc/systemd/system/runed.service
sudo systemctl daemon-reload
sudo systemctl enable --now runed
sudo systemctl status runed --no-pager
```

Step 6 — Open firewall/security group ports
- gRPC: 8080
- HTTP: 8081

Step 7 — Verify
```bash
curl -s http://localhost:7861/health || true
/usr/local/bin/runed --version || true
rune version || true
```

Notes:
- If using Amazon Linux, re-login to pick up `docker` group membership.
- For remote CLI access, set `RUNE_SERVER` to the instance public IP and ensure security groups allow your IP.

Optional flags for the installer
- `--version vX.Y.Z` to install a specific release, or `--from-source` to build from source
- `--grpc-port` and `--http-port` to override ports
- `--config` to customize config path



Troubleshotting:

sudo tail -n 200 /var/log/user-data.log
sudo docker ps           # immediate check
docker ps                # after newgrp docker or re-login
sudo journalctl -u runed -f
curl -s http://localhost:8081/health



# As admin (with working context)
rune token create \
  --name oreofe \
  --subject-id e929ee1b-41c9-4855-aeea-7a0fe8fbe2da.e7ca82e9-a8eb-4863-a920-115096ec6538 \
  --subject-type user \
  --role readwrite \
  --ttl 720h \
  --out-file /tmp/oreofe.token