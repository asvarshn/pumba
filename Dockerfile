FROM alpine:3.3

MAINTAINER Alexei Ledenev <alexei.led@gmail.com>

LABEL com.gaiaadm.pumba=true

#RUN addgroup pumba && adduser -s /bin/bash -D -G pumba pumba
RUN addgroup core && adduser -s /bin/bash -D -G core core

ENV GOSU_VERSION 1.9
RUN set -x \
    && apk add --no-cache --virtual .gosu-deps dpkg gnupg openssl ca-certificates \
    && wget -O /usr/local/bin/gosu "https://github.com/tianon/gosu/releases/download/$GOSU_VERSION/gosu-$(dpkg --print-architecture)" \
    && wget -O /usr/local/bin/gosu.asc "https://github.com/tianon/gosu/releases/download/$GOSU_VERSION/gosu-$(dpkg --print-architecture).asc" \
    && export GNUPGHOME="$(mktemp -d)" \
    && gpg --keyserver ha.pool.sks-keyservers.net --recv-keys B42F6819007F00F88E364FD4036A9C25BF357DD4 \
    && gpg --batch --verify /usr/local/bin/gosu.asc /usr/local/bin/gosu \
    && rm -r "$GNUPGHOME" /usr/local/bin/gosu.asc \
    && chmod +x /usr/local/bin/gosu \
    && gosu nobody true \
    && apk del .gosu-deps

RUN apk --update add iproute2 curl wget \
    && rm -rf /var/lib/apt/lists/* \
    && rm /var/cache/apk/*

COPY .dist/pumba /usr/bin/pumba
COPY docker_entrypoint.sh /
RUN chmod +x /docker_entrypoint.sh

ENTRYPOINT ["/docker_entrypoint.sh"]
CMD ["pumba", "--help"]a

#Ashish
