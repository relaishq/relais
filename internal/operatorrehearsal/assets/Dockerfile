FROM amazonlinux:2023.12.20260930.0
# Keep AL2023's curl-minimal provider for the template downloader.
RUN dnf install -y systemd python3 iproute procps-ng shadow-utils ethtool chrony socat findutils util-linux tar && dnf clean all
# Template management-unit shim: commands arrive through local docker exec.
RUN printf '[Unit]\nDescription=Local management-unit shim\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n' > /etc/systemd/system/amazon-ssm-agent.service
# No instance metadata. Docker owns networking; chrony uses one local reference.
RUN systemctl mask chrony-config.service systemd-networkd.service systemd-networkd.socket systemd-networkd-wait-online.service
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/lib/systemd/systemd"]
