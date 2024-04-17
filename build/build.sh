SHELL_FOLDER=$(dirname $(readlink -f "$0"))
docker build --build-arg http_proxy=$http_proxy --build-arg https_proxy=$https_proxy -t ds-processer -f $SHELL_FOLDER/Dockerfile  $SHELL_FOLDER/..