#!/bin/sh

DATE=$(date)
CONTAINERID=$(cat /proc/self/cgroup | grep '^1:'|grep -Eo '[0-9a-z]*$'|cut --bytes=-12)

echo "$DATE $NAME $CONTAINERID $*" >> /logs/log.txt

