# Humio Module for Logspout
This module allows Logspout to send Docker logs in the HEC format to Humio via TCP optionally with TLS.  We provide a way to build a container that includes [Logspout](https://github.com/gliderlabs/logspout) and the [Humio adapter](https://github.com/blockfi/logspout-humio) module so you can forward Docker logs in HEC format using `humio://hostname:port` as the Logspout command.

## Why

Because Humio is capable of accepting Splunk HEC formatted log events, but Logspout doesn't support that out of the box.  Other changed included reformatting the time into 3339 format.

## Build

To build locally for testing:
```
go get -d -v
go build -v -ldflags "-X main.Version=3.2.11"
```

Then build the docker container:
```
docker build --no-cache -t $(whoami)/logspout-humio:0.0.1 .
```

And publish it to Docker Hub:
```
docker push $(whoami)/logspout-humio:0.0.1
```

Or archive and distribute it by hand as a `tar` file you will need to save the Docker image,
then copy it to some new host (`scp`, `rsync`, ...) and load it.
```
docker save -o /tmp/logspout-humio.tar $(whoami)/logspout-humio:0.0.1
scp /tmp/logspout-humio.tar user@host.example.com:/tmp
```

Then on that remote host:
```
ssh user@hostexample.com
docker load -i /tmp/logspout-humio.tar
```

Or do it in one step:
```
docker save $(whoami)/logspout-humio:0.0.1 | bzip2 | pv | ssh user@host.example.com 'bunzip2 | docker load'
```

## Examples

### Command Line

If the load-balancer for Humio is accepting HTTPS requests on 443 for the API then
you need to include the port (443) as below.

When testing:

```
docker run \
    --rm \
	--env HUMIO_INGEST_TOKEN=h9ghusufdghsfghsfghosfhg --env HUMIO_DOCKER_LABELS=true --env DEBUG=1 \
    --name logspout-humio \
	--hostname $(hostname -f) \
	--volume=/var/run/docker.sock:/var/run/docker.sock \
	--publish=127.0.0.1:8000:80 \
	$(whoami)/logspout-humio:0.0.1 \
	humio://cloud.us.humio.com:443
```

You can run `curl http://127.0.0.1:8000/logs` to view what Logspout is shipping
to Humio.


As a daemon, for production/daily unattended use:
```
docker run \
    --detach \
	--env HUMIO_INGEST_TOKEN=h9ghusufdghsfghsfghosfhg --env HUMIO_DOCKER_LABELS=true \
    --name logspout-humio \
	--hostname $(hostname -f) \
	--volume=/var/run/docker.sock:/var/run/docker.sock \
	--restart=unless-stopped \
	$(whoami)/logspout-humio:0.0.1 \
	humio://cloud.us.humio.com:443
```

### Docker Compose

You could use this image with the following docker-compose file:

```
version: '2'

services:
  logspout:
    image: blockfi/logspout-humio:latest
    hostname: my.message.source
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command: humio://cloud.us.humio.com:443
    restart: unless-stopped
```

Don't forget to prune containers on your system `docker system prune --volumes`.

## Configuration

### Logspout
 * `http.proxy` - proxy URI for requests
 * `http.buffer.capacity` - bytes of log data to buffer before shipping it to Humio
 * `http.buffer.timeout` - timeout after which log data ships to Humio regardless of amount buffered
 * `http.gzip` - compress data for shipping
 * `http.crash` - crash or error message on failed request

### Environment Variables
 * `HUMIO_INGEST_TOKEN`: the ingest token created for the repository you're targeting, required and unset.
 * `HUMIO_TLS`: `true` - when true we expect the endpoint to require TLS authentication
 * `HUMIO_HEC_PATH`: `/api/v1/ingest/hec` - appended to the URI, Humio [HEC](https://docs.humio.com/integrations/data-shippers/hec/) supports both the standard (`/services/collector`) and this Humio-specific.
 * `HUMIO_SOURCETYPE`: `docker` - the `sourcetype` field in the JSON for each event will contain this value
 * `HUMIO_DOCKER_LABELS`: - include the docker container labels in the events
 * `DEBUG`: `false` - when true output more debugging messages

## License
ASLv2. See [License](LICENSE)

## Disclaimer

This image provided to you as-is, yadda yadda etc. YMMV, and that's not our fault. :)
