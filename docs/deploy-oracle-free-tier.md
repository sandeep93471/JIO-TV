# Deploy JioTV Go on Oracle Cloud Always Free (24/7, India)

Oracle's **Always Free** tier gives a permanent ARM VM (Ampere A1: 1 OCPU,
6 GB RAM) at zero cost. Deploy in an **Indian region** so Jio's geo-restricted
APIs and streams work.

## 1. Create the OCI account

1. Go to <https://www.oracle.com/cloud/free/> → **Start for free**.
2. **IMPORTANT — Home Region**: Always-Free resources live only in your home
   region and it can't be changed later. Pick **India West (Mumbai)** or
   **India South (Hyderabad)** — Jio requires an Indian IP.
3. A credit card is needed for identity verification (not charged on Always
   Free).

## 2. Create the VM

Compute → Instances → **Create instance**:

- **Name**: `jiotv`
- **Image**: Ubuntu 24.04 (aarch64) or Oracle Linux 9
- **Shape**: `VM.Standard.A1.Flex` — 1 OCPU / 6 GB RAM (Always Free)
- **Networking**: new VCN is fine; make sure **Assign a public IPv4 address**
  is checked
- **SSH keys**: let OCI generate one and **download the private key** (or paste
  your own public key)

If Mumbai shows "Out of capacity for shape VM.Standard.A1.Flex", try a
different availability domain or retry later — free capacity fluctuates.

## 3. Open port 5001 (two places — both required)

**A. OCI Security List:**

Networking → your VCN → Subnets → Default Security List →
**Add Ingress Rules**:

- Stateless: No
- Source CIDR: `0.0.0.0/0`
- Protocol: TCP, Destination port: `5001`

**B. VM firewall** (OCI Ubuntu images ship an iptables REJECT rule):

```bash
sudo iptables -I INPUT 6 -m state --state NEW -p tcp --dport 5001 -j ACCEPT
sudo apt-get install -y iptables-persistent   # keeps it across reboots
```

(Oracle Linux uses firewalld instead:
`sudo firewall-cmd --permanent --add-port=5001/tcp && sudo firewall-cmd --reload`)

## 4. Install Docker + deploy

```bash
# Docker
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker $USER && newgrp docker

# Build + run (from your GitHub repo)
git clone https://github.com/sandeep93471/JIO-TV.git
cd JIO-TV
docker build -t jiotv .
docker run -d --name jiotv \
  -p 5001:5001 \
  -v jiotv_data:/app/.jiotv_go \
  --restart unless-stopped \
  jiotv
```

The `jiotv_data` volume persists credentials, the premium search index, and
continue-watching data across container restarts.

## 5. Log in

Open `http://<VM-PUBLIC-IP>:5001` → **Login** → enter your Jio mobile number →
OTP. Credentials are stored in the volume, so this is a one-time step.

The premium search index builds in the background on first run (~2 min) and
then persists to disk.

## Managing it

```bash
docker logs -f jiotv        # watch logs
docker restart jiotv        # restart
cd JIO-TV && git pull && docker build -t jiotv . && \
  docker rm -f jiotv && \
  docker run -d --name jiotv -p 5001:5001 -v jiotv_data:/app/.jiotv_go \
    --restart unless-stopped jiotv     # update to latest code
```

## Optional: HTTPS + domain

Point a domain at the VM's public IP, then front it with Caddy (one line in
`Caddyfile`: `yourdomain.com { reverse_proxy localhost:5001 }`) for automatic
HTTPS. Without a domain, plain `http://IP:5001` works fine.
