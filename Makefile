.PHONY: $(shell ls)

machine_dir=/var/lib/machines/k-ubuntu
ubuntu_release=impish

build: bin/k
bin/k: $(shell find . -type f -name '*.go')
	mkdir -p bin
	go build -o bin/k ./cmd/k

test:
	./testdata/integration/run

start-quiet:
	sudo systemd-nspawn \
		--machine k  \
		--boot \
		--ephemeral \
		--directory $(machine_dir) \
		--console pipe \
		--quiet

start:
	sudo systemd-nspawn \
		--machine k  \
		--boot \
		--ephemeral \
		--bind "$(PWD):/k" \
		--directory $(machine_dir)

stop:
	(sudo machinectl | grep "k " && sudo machinectl terminate k) || true

ssh:
	ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@localhost

machine:
	sudo apt update && sudo apt install -y systemd-container debootstrap btrfs-progs
	test -f /var/lib/machines.raw || (\
		sudo truncate -s 2G /var/lib/machines.raw &&\
		sudo mkfs -t btrfs /var/lib/machines.raw &&\
		sudo mount -o loop -t btrfs /var/lib/machines.raw /var/lib/machines\
	)
	sudo rm -rf $(machine_dir)
	sudo debootstrap \
		--include=systemd-container,openssh-server,git,make,golang-go \
		$(ubuntu_release) $(machine_dir)
	sudo systemd-nspawn -D $(machine_dir) -u root -- bash -c '\
		echo "root:hunter2" | chpasswd &&\
		mkdir -p ~/.ssh &&\
		echo "$(shell cat ~/.ssh/id*.pub)" > ~/.ssh/authorized_keys &&\
		sed "s@session\s*required\s*pam_loginuid.so@session optional pam_loginuid.so@g" -i /etc/pam.d/sshd'
