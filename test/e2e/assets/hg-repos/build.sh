#!/bin/bash

rm -rf build

mkdir build
cd build

mkdir repo
cd repo

hg init .

echo "content1" > file1.txt

hg add file1.txt
hg commit -m "Added file1"
CSET1_ID=$(hg id --id)

hg tag first-tag
hg phase -p

hg topic "wip"
echo "content2" > file1.txt
hg commit -m "extra cset"
CSETX_ID=$(hg id --id)

hg strip -r . 

BUNDLE=$(basename .hg/strip-backup/*-backup.hg)
mv .hg/strip-backup/$BUNDLE ..

hg checkout 00000

cd ..

cat > info.json <<EOF
{
    "initial-changeset": "$CSET1_ID",
    "extra-bundle": "$BUNDLE",
    "extra-changeset": "$CSETX_ID"
}
EOF

tar caf ../asset.tgz .
