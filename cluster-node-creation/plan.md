# Kubernetes from Scratch — Learning Plan

## What Kubernetes Actually Is

Before touching a VM, you need a mental model.

Kubernetes is a **distributed system** that manages containers across multiple machines. At its core, it is:

1. A **database** (etcd) that stores the desired state of everything
2. A **reconciliation loop** — controllers constantly compare *desired state* vs *actual state* and act to close the gap
3. A **set of agents** on each machine that carry out instructions

That's it. Everything else is details on top of this.

---

## The Components

### Control Plane (master node)

| Component | What it does |
|---|---|
| **etcd** | Distributed key-value store. The single source of truth. If this dies, the cluster is blind. |
| **kube-apiserver** | The only component that talks to etcd. Everything else talks to the API server. It is the front door. |
| **kube-controller-manager** | Runs dozens of controllers in one process. Each controller watches the API server and reconciles state (e.g. "3 replicas requested, 2 running → start 1 more"). |
| **kube-scheduler** | Watches for unscheduled pods, picks a node for each based on resource requests, affinity rules, taints, etc. It writes the decision back to the API server — it does NOT start the pod itself. |

### Worker Nodes (every node, including master in dev setups)

| Component | What it does |
|---|---|
| **kubelet** | The node agent. Watches the API server for pods assigned to its node, then instructs the container runtime to start/stop them. It also reports node and pod status back. |
| **kube-proxy** | Manages iptables/IPVS rules on the node to implement Service networking (ClusterIP, NodePort). It's how traffic actually reaches your pods. |
| **Container runtime** | Does the actual work of pulling images and running containers. Kubernetes talks to it via the **CRI** (Container Runtime Interface). We use **containerd**. |

### The CNI Plugin (networking layer)

Kubernetes defines a spec — the **Container Network Interface** — but ships with no networking. You must install a plugin. The plugin is responsible for:
- Giving every pod a unique IP
- Making pods reachable across nodes

We use **Flannel**, which uses VXLAN overlay networking to tunnel pod traffic between nodes.

---

## The Flow When You Start a Pod

Understanding this sequence is worth more than any tutorial:

```
You run: kubectl create deployment nginx --image=nginx

1. kubectl → sends HTTP POST to kube-apiserver
2. kube-apiserver → validates & persists the Deployment object to etcd
3. Deployment controller (in controller-manager) → sees new Deployment, creates a ReplicaSet
4. ReplicaSet controller → sees new ReplicaSet, creates a Pod object (status: Pending, no node assigned)
5. kube-scheduler → sees unscheduled Pod, scores nodes, picks one, writes nodeName to Pod spec in etcd
6. kubelet on that node → sees a Pod assigned to it, calls containerd to pull image and start container
7. kubelet → updates Pod status to Running in etcd via API server
8. kube-proxy → updates iptables rules so the Service can route to the new pod IP
```

Every step is an independent actor watching the API server. Nothing is directly coupled. This is why Kubernetes is resilient — any component can restart and pick up where it left off.

---

## Setup Guide

### Phase 1 — Create the VMs

Install OrbStack (replaces Multipass — better Apple Silicon support, no networking issues):

```bash
brew install --cask orbstack
```

Before creating VMs, set global resource limits via **OrbStack app → Settings → Resources**: 4 CPUs, 4GB memory, 30GB disk (shared across both VMs). Then:

```bash
orb create ubuntu:24.04 k8s-master
orb create ubuntu:24.04 k8s-worker
```

Open two shells, one per VM:

```bash
orb -m k8s-master
orb -m k8s-worker
```

Run all Phase 2 and 3 commands on both VMs unless stated otherwise.

---

### Phase 2 — Prepare the OS (both nodes)

```bash
# Disable swap — Kubernetes requires this.
# The kubelet's memory accounting assumes swap is off.
# With swap, the scheduler can't make accurate resource decisions.
sudo swapoff -a
sudo sed -i '/ swap / s/^/#/' /etc/fstab
```

```bash
# Load kernel modules needed for container networking
sudo modprobe overlay        # used by containerd for layered filesystems
sudo modprobe br_netfilter   # allows iptables to see bridged traffic

# Make them load on boot
cat <<EOF | sudo tee /etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF
```

```bash
# Allow iptables to see bridged IPv4/IPv6 traffic.
# Without this, kube-proxy's iptables rules won't intercept pod traffic
# crossing the bridge between containers and the host network.
cat <<EOF | sudo tee /etc/sysctl.d/k8s.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF

sudo sysctl --system
```

---

### Phase 3 — Install containerd (both nodes)

Why containerd and not Docker: Kubernetes removed the Docker-specific shim (dockershim) in v1.24. It now requires a CRI-compliant runtime. containerd *is* what Docker uses internally — you're just cutting out the Docker daemon layer.

```bash
sudo apt-get update
sudo apt-get install -y containerd
```

Configure containerd to use the systemd cgroup driver. If containerd and kubelet use different cgroup drivers, the kubelet will fail to start pods.

```bash
sudo mkdir -p /etc/containerd
containerd config default | sudo tee /etc/containerd/config.toml

# Switch cgroup driver to systemd
sudo sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml

sudo systemctl restart containerd
sudo systemctl enable containerd
```

---

### Phase 4 — Install kubeadm, kubelet, kubectl (both nodes)

```bash
sudo apt-get install -y apt-transport-https ca-certificates curl gpg

curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key | \
  sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg

echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.30/deb/ /' | \
  sudo tee /etc/apt/sources.list.d/kubernetes.list

sudo apt-get update
sudo apt-get install -y kubelet kubeadm kubectl

# Pin versions — auto-upgrades would break the cluster
sudo apt-mark hold kubelet kubeadm kubectl
```

What each tool is:
- `kubeadm` — a bootstrapping tool. You use it once to set up the cluster. It is not a runtime component.
- `kubelet` — the agent that runs on every node forever. It is a systemd service.
- `kubectl` — your CLI to talk to the API server. Used from wherever you want to manage the cluster.

---

### Phase 5 — Initialize the control plane (master only)

```bash
# Get the master's IP first
hostname -I | awk '{print $1}'
```

```bash
sudo kubeadm init \
  --pod-network-cidr=10.244.0.0/16 \
  --apiserver-advertise-address=<MASTER_IP>
```

`--pod-network-cidr`: The IP range Kubernetes will use for pod IPs. Must match what your CNI plugin expects. Flannel expects `10.244.0.0/16`.

What kubeadm init does internally:
1. Generates all TLS certificates (CA, apiserver cert, kubelet client cert, etcd cert, etc.) and stores them in `/etc/kubernetes/pki/`
2. Generates kubeconfig files for each component so they can authenticate to the apiserver
3. Starts etcd, apiserver, controller-manager, and scheduler as **static pods** — YAML manifests in `/etc/kubernetes/manifests/` that the kubelet watches and manages
4. Installs CoreDNS and kube-proxy
5. Prints the join command for worker nodes

Set up kubectl access:

```bash
mkdir -p $HOME/.kube
sudo cp /etc/kubernetes/admin.conf $HOME/.kube/config
sudo chown $(id -u):$(id -g) $HOME/.kube/config
```

Check the control plane — nodes show `NotReady` because there is no CNI yet:

```bash
kubectl get nodes
kubectl get pods -n kube-system
```

---

### Phase 6 — Install Flannel CNI (master only)

```bash
kubectl apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml
```

```bash
kubectl get pods -n kube-flannel --watch
```

Once running, the master node flips to `Ready`. Flannel creates a virtual network interface (`flannel.1`) on each node and uses VXLAN to encapsulate pod-to-pod traffic in UDP packets.

---

### Phase 7 — Join the worker (worker node only)

kubeadm printed a join command at the end of `init`:

```bash
sudo kubeadm join <MASTER_IP>:6443 \
  --token <token> \
  --discovery-token-ca-cert-hash sha256:<hash>
```

Then on the master:

```bash
kubectl get nodes --watch
```

Both nodes should reach `Ready` within about 60 seconds.

---

## Exercises (in order)

1. **Deploy a pod and trace it** — create a pod YAML, apply it, then use `kubectl describe pod` to read the event log. You'll see the scheduler assign it and the kubelet start it.

2. **Look at etcd directly** — `kubectl exec` into the etcd pod and use `etcdctl` to read raw cluster state. You'll see every object in its serialised form.

3. **Read the static pod manifests** — `cat /etc/kubernetes/manifests/kube-apiserver.yaml`. This is how the control plane runs. No magic.

4. **Drain the worker** — `kubectl drain k8s-worker --ignore-daemonsets` and watch pods reschedule. Then `kubectl uncordon` it.

5. **Break something on purpose** — stop containerd on the worker with `sudo systemctl stop containerd` and watch the node go `NotReady`, then watch pods get evicted and rescheduled.

---

## Topics to Go Deeper On

- Networking internals (VXLAN, iptables rules, how ClusterIP works)
- The scheduler algorithm (scoring, filtering, priorities)
- etcd internals (Raft consensus, how leader election works)
- TLS bootstrapping (how kubelets authenticate)
- RBAC (how authorization works for every API call)
