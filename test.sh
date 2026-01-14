#!/bin/bash

/client &
sleep 2
chmod +x adb
cd /tmp
#wget https://nexus.rnd.lanit.ru/repository/share/1765353385150_1758535805244_ApiDemos-debug.apk
#mv 1765353385150_1758535805244_ApiDemos-debug.apk ApiDemos.apk
echo "Открываем порт 8200"
adb forward tcp:8200 tcp:6790
sleep 5
netstat -tuln
sleep 5
echo "Закрываем порт 8200"
adb forward --remove tcp:8200
sleep 5
netstat -tuln

echo "Открываем порт 8200"
adb forward tcp:8200 tcp:6790
sleep 5
netstat -tuln
sleep 5
echo "Закрываем порт 8200"
adb forward --remove tcp:8200
sleep 5
netstat -tuln
#adb install ApiDemos.apk