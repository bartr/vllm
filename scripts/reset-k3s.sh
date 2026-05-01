#!/bin/bash

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$script_dir" || exit 1

# Target size for the root LV that backs k3s (/var/lib/rancher).
# Override with: ROOT_LV_SIZE=500G ./reset-k3s.sh
ROOT_LV_SIZE="${ROOT_LV_SIZE:-250G}"
ROOT_LV_PATH="${ROOT_LV_PATH:-/dev/ubuntu-vg/ubuntu-lv}"

sudo /usr/local/bin/k3s-uninstall.sh
rm -f ~/.kube/config
sudo rm -rf /etc/rancher/k3s

# optional aggressive wipe if you want to remove all cluster state, images, PVC data, and CNI leftovers
#sudo rm -rf /var/lib/rancher /var/lib/kubelet /etc/cni /var/lib/cni

# Resize the root LV to the target size before bringing k3s back up.
# - Growing is online-safe (lvextend + resize2fs).
# - Shrinking is skipped here because it requires unmounting / and must be
#   done from a rescue/live environment. If the LV is already larger than
#   $ROOT_LV_SIZE, this script leaves it alone.
if command -v lvs >/dev/null 2>&1 && sudo lvs --noheadings "$ROOT_LV_PATH" >/dev/null 2>&1; then
    current_bytes=$(sudo lvs --noheadings --nosuffix --units b -o lv_size "$ROOT_LV_PATH" | awk '{print $1}')
    target_bytes=$(numfmt --from=iec "${ROOT_LV_SIZE}")
    if [ "$current_bytes" -lt "$target_bytes" ]; then
        echo "==> Growing $ROOT_LV_PATH to $ROOT_LV_SIZE"
        sudo lvextend -L "$ROOT_LV_SIZE" "$ROOT_LV_PATH" || \
            sudo lvextend -l +100%FREE "$ROOT_LV_PATH"
        sudo resize2fs "$ROOT_LV_PATH"
    elif [ "$current_bytes" -gt "$target_bytes" ]; then
        echo "==> $ROOT_LV_PATH is larger than target $ROOT_LV_SIZE (current: $(numfmt --to=iec --suffix=B "$current_bytes"))."
        echo "    Skipping shrink — shrinking the mounted root FS is not safe online."
        echo "    To shrink: boot a live/rescue environment and run:"
        echo "      e2fsck -f $ROOT_LV_PATH && resize2fs $ROOT_LV_PATH ${ROOT_LV_SIZE} && lvreduce -L ${ROOT_LV_SIZE} $ROOT_LV_PATH"
    fi
    df -h / | tail -1
fi

curl -sfL https://get.k3s.io | sh -

sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/config
sudo chown $(id -u):$(id -g) -R ~/.kube

cd ../cllm
make deploy

# wait for the node to be ready
kubectl wait --for=condition=Ready node/z01 --timeout=3m

# give K3s a chance to start
sleep 25

# wait for traefik to be ready
kubectl -n kube-system rollout status deployment/traefik --timeout=3m
