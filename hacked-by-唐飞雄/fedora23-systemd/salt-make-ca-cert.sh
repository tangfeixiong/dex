#!/bin/bash

set -e

CERT_DIR=$HOME
CERT_GRP=$(id -gn)

case $1 in
    dex-v2)
        ARG_CERT_IP="192.168.1.100"
        K8S_CLUSTER_IP="10.123.240.15"
        EXTRA_SANS=(
            IP:$ARG_CERT_IP
            IP:$K8S_CLUSTER_IP
            IP:10.64.33.81
            IP:10.64.33.90
            IP:10.64.33.224
            DNS:www.10.64.33.81.xip.io
            DNS:www.10.64.33.90.xip.io
            DNS:www.10.64.33.224.xip.io
            DNS:dex.default.svc.cluster.local
          )
        ARG_EXTRA_SANS=$(echo "${EXTRA_SANS[@]}" | tr ' ' ,)
        echo $ARG_EXTRA_SANS
        CERT_DIR+="/.pki/dex_v2"
        echo "Create ca cert into $CERT_DIR"

        debug='true' CERT_DIR=$CERT_DIR CERT_GROUP=$CERT_GRP $(dirname "$BASH_SOURCE[0]")/kubernetes/saltbase/salt/generate-cert/make-ca-cert.sh $ARG_CERT_IP $ARG_EXTRA_SANS

        mv $CERT_DIR/server.cert $CERT_DIR/tls.crt
        mv $CERT_DIR/server.key $CERT_DIR/tls.key
        mv $CERT_DIR/kubecfg.crt $CERT_DIR/admin.crt
        mv $CERT_DIR/kubecfg.key $CERT_DIR/admin.key

        ;;
    coreos-dex,dex*)
        user_group=$(grep "kube-cert" /etc/group)
        if [ -z $user_group ]; then
            sudo groupadd -f -r kube-cert
        fi

        ARG_CERT_IP="10.64.33.90"

        EXTRA_SANS=(
            IP:$ARG_CERT_IP
            IP:10.0.2.15
            IP:127.0.0.1
            DNS:www.10.64.33.90.xip.io
            DNS:accounts.10.64.33.90.xip.io
          )

        ARG_EXTRA_SANS=$(echo "${EXTRA_SANS[@]}" | tr ' ' ,)

        echo $ARG_EXTRA_SANS
        echo "Create ca cert into /srv/kubernetes"

        $(dirname "$BASH_SOURCE[0]")/kubernetes/saltbase/salt/generate-cert/make-ca-cert.sh "$ARG_CERT_IP" "$ARG_EXTRA_SANS"
		
		echo "Remember to copy destination certs into /etc/coreos/dex for start service"

        ;;
    kubernetes)
        MASTER_IP="10.64.33.81"
        # Same as GKE, cluster CIDR: 10.120.0.0/14, service CIDR: 10.123.240.0/20
        MASTER_SERVICE="10.123.240.1"

        EXTRA_SANS=(
            IP:$MASTER_IP
            IP:$MASTER_SERVICE
            DNS:kubernetes
            DNS:kubernetes.default
            DNS:kubernetes.default.svc
            DNS:kubernetes.default.svc.cluster.local
          )

        ARG_CERT_IP=$MASTER_IP
        ARG_EXTRA_SANS=$(echo "${EXTRA_SANS[@]}" | tr ' ' ,)

        echo $ARG_EXTRA_SANS
        echo "Generate ca certs into $CERT_DIR"

        debug='true' CERT_DIR=$CERT_DIR CERT_GROUP=$CERT_GRP $HOME/kubernetes/saltbase/salt/generate-cert/make-ca-cert.sh $ARG_CERT_IP $ARG_EXTRA_SANS

        ;;
    docker-registry*,docker-distribution)
        lc-tlscert
        ;;

    *)
        echo "Usage: $0 <options>

options:
    kubernetes
    dex-v2
    dex-v1
    docker-registry
"

        ;;
esac
