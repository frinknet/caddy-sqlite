#!/bin/sh

{
  IFS= read -r L1;
  IFS= read -r L2;
  IFS= read -r L3;
} < ./NOTICE

NOTICE=$(tail -n +4 ./NOTICE)
REPOFROM=${L1#*: }             # Full Repo Origin 
REPOSLUG=${REPOFROM%-template} # Type Code Name
REPOPREF=${REPOSLUG%%-*}-      # Type Prefix
REPOTYPE=${L1%%: *}            # Type Friendly Name

COPYBASE=2${L2#* 2}            # Copyight Base
COPYYEAR=${COPYBASE%% *}       # Copyight Year
COPYHOLD=${COPYBASE#* }        # Copyight Holder
COPYSPDX=${L3#*: }             # License SPDX

INIT=0
for arg in "$@"; do
  case $arg in
    --) shift; break;;
    --init) INIT=1; shift;;
    *) shift; break;;
  esac
done

if [ "$INIT" -eq 1 ]; then
  LICENSES="MIT Apache-2.0 BSD-3-Clause 0BSD GPL-2.0-only AGPL-3.0-later"
  
  echo "$LICENSES" | tr " " "\n - "
  printf "License: %s"  "$COPYSPDX"; IFS= read -r IN_SPDX

  [ -z "$IN_SPDX" ] && IN_SPDX=$COPYSPDX

  case " $LICENSES " in
    *" $IN_SPDX "*) ;;
    *) echo "invalid license: $IN_SPDX" >&2 ; exit 1;;
  esac

  printf "Copyright: %s" "$COPYHOLD"; IFS= read -r IN_HOLD

  [ -z "${IN_HOLD}" ] && IN_HOLD="${COPYHOLD}"

  printf "Repo Name: %s"  "$REPOPREF"; IFS= read -r IN_REPO

  if [ -z "${IN_REPO}" ]; then
    if [ "${REPOFROM}" = "${REPOSLUG}" ]; then
      SLUG=${REPOSLUG}
    else
      echo "missing repo name" >&2;
      exit 1;
    fi
  else
    SLUG=${REPOPREF}${IN_REPO}
    touch ./${IN_REPO}.go
    mkdir -p pkg/ www/
  fi
  
  SPDX="${IN_SPDX}"
  HOLD="${IN_HOLD}"
else
  SLUG=${REPOSLUG}
  SPDX="${COPYSPDX}"
  HOLD="${COPYHOLD}"
fi

case $SLUG in
  *[!A-Za-z0-9-]*) echo "invalid repo name... Run --init again" >&2; exit 1;;
esac

copyyear() {
  _pre=${1%%[-,]*}
  _now=$(date +%Y)
  [ "$_pre" -eq "$_now" ] || _now="$_pre-$_now"
  echo "$_now"
}

YEAR=$(copyyear "${COPYYEAR}")

{
  echo "${REPOTYPE}: ${SLUG}"
  echo "Copyright © ${YEAR} ${HOLD}"
  echo "Released under license: ${SPDX}"
  echo "${NOTICE}"
} > ./NOTICE

sed -i -e "s|${REPOFROM}|${SLUG}|g" ./Dockerfile
sed -i -e "s|${REPOFROM}|${SLUG}|g" ./go.mod

if [ "$INIT" -eq 0 ]; then
  docker rmi "${REPOSLUG}" 2>/dev/null || true
  docker rmi "${SLUG}" 2>/dev/null || true
  docker build -t "${SLUG}:test" .
fi
