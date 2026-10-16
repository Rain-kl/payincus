FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    linux-image-virtual qemu-system-x86 iproute2 nftables busybox-static \
    e2fsprogs ca-certificates && rm -rf /var/lib/apt/lists/*
RUN apt-get update && set -- /lib/modules/* \
    && apt-get install -y --no-install-recommends netbase "linux-modules-extra-${1##*/}" \
    && rm -rf /var/lib/apt/lists/*
COPY egress.test /egress.test
COPY egress-datapath-init /test-init
RUN chmod 755 /egress.test /test-init && mkdir /vm-root \
    && tar -C / --exclude=usr/lib/firmware -cf - bin boot etc lib lib64 sbin usr test-init egress.test \
       | tar --no-same-owner -C /vm-root -xf -
RUN mkdir -p /vm-root/proc /vm-root/sys /vm-root/dev /vm-root/run /vm-root/tmp /vm-root/var/lib \
    && truncate -s 2G /root.img && mkfs.ext4 -q -F -d /vm-root /root.img \
    && rm -rf /vm-root
ENTRYPOINT ["sh", "-ec", "exec qemu-system-x86_64 -machine accel=tcg -m 1536 -smp 2 -kernel /boot/vmlinuz -append 'root=/dev/vda rw console=ttyS0 init=/test-init quiet panic=-1' -drive file=/root.img,format=raw,if=virtio -nographic -no-reboot -nic none"]
