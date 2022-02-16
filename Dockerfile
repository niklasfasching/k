FROM ubuntu:latest

RUN apt-get update && apt-get install -y systemd openssh-server

# https://github.com/containers/podman/issues/3651
RUN sed 's@session\s*required\s*pam_loginuid.so@session optional pam_loginuid.so@g' -i /etc/pam.d/sshd

COPY etc/id.pub /root/.ssh/authorized_keys

CMD ["/lib/systemd/systemd"]
