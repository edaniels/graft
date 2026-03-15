FROM ubuntu:24.04

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        openssh-server \
        bash \
        ca-certificates \
        iproute2 \
    && rm -rf /var/lib/apt/lists/*

# Configure SSH server.
RUN mkdir -p /run/sshd && \
    sed -i 's/#PermitRootLogin .*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config && \
    sed -i 's/#PubkeyAuthentication .*/PubkeyAuthentication yes/' /etc/ssh/sshd_config && \
    sed -i 's/#PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config

# Create a test user with a home directory.
RUN useradd -m -s /bin/bash testuser

# The SSH authorized_keys file will be populated at runtime by the test harness
# via docker cp, so we just prepare the directory here.
RUN mkdir -p /home/testuser/.ssh && \
    chmod 700 /home/testuser/.ssh && \
    chown -R testuser:testuser /home/testuser/.ssh

EXPOSE 22

# Start the SSH daemon in the foreground.
CMD ["/usr/sbin/sshd", "-D", "-e"]
