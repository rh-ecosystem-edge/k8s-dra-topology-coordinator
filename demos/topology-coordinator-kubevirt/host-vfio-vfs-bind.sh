#!/usr/bin/env bash
set -euo pipefail

vfio_driver=/sys/bus/pci/drivers/vfio-pci
vfs=()
gim_pfs=()

for pf in /sys/bus/pci/drivers/gim/*; do
  [[ -e "$pf" ]] || continue
  [[ "$(basename "$pf")" =~ ^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$ ]] || continue
  gim_pfs+=("$(basename "$pf")")
done

for device in /sys/bus/pci/devices/*; do
  [[ -f "$device/vendor" && -f "$device/device" ]] || continue
  [[ "$(<"$device/vendor")" == "0x1002" ]] || continue
  [[ "$(<"$device/device")" == "0x74b5" ]] || continue
  [[ -L "$device/physfn" ]] || continue

  parent=$(basename "$(readlink "$device/physfn")")
  parent_driver=$(basename "$(readlink "/sys/bus/pci/devices/$parent/driver")")
  [[ "$parent_driver" == "gim" ]] || {
    echo "parent $parent is bound to $parent_driver, expected gim" >&2
    exit 1
  }
  vfs+=("$(basename "$device")")
done

(( ${#vfs[@]} == ${#gim_pfs[@]} )) || {
  echo "found ${#vfs[@]} GPU VFs for ${#gim_pfs[@]} GIM PFs" >&2
  exit 1
}

/usr/sbin/modprobe vfio-pci

for bdf in "${vfs[@]}"; do
  device="/sys/bus/pci/devices/$bdf"
  current_driver=""
  if [[ -L "$device/driver" ]]; then
    current_driver=$(basename "$(readlink "$device/driver")")
  fi

  [[ "$current_driver" == "vfio-pci" ]] && continue

  printf '%s\n' vfio-pci > "$device/driver_override"
  if [[ -n "$current_driver" ]]; then
    printf '%s\n' "$bdf" > "/sys/bus/pci/drivers/$current_driver/unbind"
  fi
  printf '%s\n' "$bdf" > "$vfio_driver/bind"
  printf '\n' > "$device/driver_override"
done

for bdf in "${vfs[@]}"; do
  driver=$(basename "$(readlink "/sys/bus/pci/devices/$bdf/driver")")
  [[ "$driver" == "vfio-pci" ]] || {
    echo "$bdf is bound to $driver, expected vfio-pci" >&2
    exit 1
  }
done
