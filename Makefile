.PHONY: clean all deploy

clean:
	rm -f api

all: api 

api:
	GO111MODULE=on go build api.go

deploy:
	rsync -rvhz --progress api root@api.ausdomainledger.net:/root
	ssh root@api.ausdomainledger.net "systemctl stop api; cp /root/api /home/api/api; chown api:api /home/api/api; setcap CAP_NET_BIND_SERVICE=+eip /home/api/api; systemctl start api;"
