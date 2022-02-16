.PHONY: $(shell ls)

start: stop
	rm -f etc/id* && ssh-keygen -q -f etc/id -P ""
	sudo podman build --tag server .
# https://github.com/containers/podman/issues/3651
	sudo podman run --detach --rm --cap-add audit_write --name server server
	sleep 1
	$(MAKE) ssh

ssh:
	ssh -i etc/id \
		-o StrictHostKeyChecking=no \
		-o UserKnownHostsFile=/dev/null \
		root@$$(sudo podman inspect server | jq -r '.[] | .NetworkSettings.IPAddress')

stop:
	sudo podman kill server || true
