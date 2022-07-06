#!/bin/bash

ts(){
    date +%T
}

cleanup(){
    rm -f ./mirrored_*
}

if [ $# -eq 0 ]
  then
    echo "No arguments supplied."
    echo " Usage: ./run-mirror-traffic.sh -n|--num-of-calls INTEGER -m|--mode [rolling|static]"
    echo " where num-of-calls sets the number of parallel magic mirror sessions and mode decides if sessions are recreated or not."
fi


APPLICATION_SERVER_PORT=8443
APPLICATION_SERVER_ADDR=$(kubectl get svc webrtc-server -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
TURN_SERVER_ADDR=$(kubectl get svc stunner -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
TURN_SERVER_PORT=$(kubectl get cm stunner-config -n default -o jsonpath='{.data.STUNNER_PORT}')

POSITIONAL_ARGS=()
while [[ $# -gt 0 ]]; do
  case $1 in
    -n|--num-of-calls)
      NUM_OF_CALLS="$2"
      shift # past argument
      shift # past value
      ;;
    -m|--mode)
      MODE="$2"
      shift # past argument
      shift # past value
      ;;
    --default)
      DEFAULT=YES
      shift # past argument
      ;;
    -*|--*)
      echo "Unknown option $1"
      exit 1
      ;;
    *)
      POSITIONAL_ARGS+=("$1") # save positional arg
      shift # past argument
      ;;
  esac
done

set -- "${POSITIONAL_ARGS[@]}" # restore positional parameters

cleanup

MIRROR_GO_CMD="go run ../webrtc-client-magic-mirror.go --turn="turn:${TURN_SERVER_ADDR}:${TURN_SERVER_PORT}" --url="wss://${APPLICATION_SERVER_ADDR}:${APPLICATION_SERVER_PORT}/magicmirror" --debug -file=../video-samples/mate-sample.ivf"

CALL_ID=0
while [ $CALL_ID -lt $NUM_OF_CALLS ];
do
    echo "[$(ts)] Setting up call with id: $CALL_ID"
    $MIRROR_GO_CMD &> call_$CALL_ID.log &
    pids[${CALL_ID}]=$! #store the parent pid

    ((CALL_ID=CALL_ID+1))
    sleep 1
done

# wait for all pids
for pid in ${pids[*]}; do
    wait $pid
done

echo "[$(ts)] All calls are done. Exit."
