#!/bin/bash

DATE=$(date)
CONTAINERID=$(basename $(cat proc/self/cpuset) | cut -c 1-12)
LOG="/logs/log.txt"

case $1 in
sleep)
	trap "echo $DATE $NAME $CONTAINERID Received SIGTERM >> $LOG; exit 0" SIGTERM
	echo "$DATE $NAME $CONTAINERID Starting" >> $LOG

	sleep 5
	touch /tmp/ready

	while true
	do
		sleep 1
	done
	;;
*)
	echo "$DATE $NAME $CONTAINERID $*" >> $LOG
	;;
esac
