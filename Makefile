.PHONY: $(shell ls)

machine_dir=/var/lib/machines/k-ubuntu
ubuntu_release=impish

start:
	sudo systemd-nspawn \
		--machine k  \
		--boot \
		--ephemeral \
		--bind "$(PWD):/k" \
		--bind "$(PWD)/testdata/out:/usr/local/lib/systemd/system" \
		--directory $(machine_dir)

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

test-deploy:
	@git init testdata/remote
	@cd testdata/remote && git config receive.denyCurrentBranch updateInstead
	@ln -rsf ./k testdata/remote/.git/hooks/update
	go build ./cmd/k
	git push ./testremote
