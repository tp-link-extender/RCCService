FROM mcr.microsoft.com/windows/nanoserver:ltsc2022-amd64
WORKDIR /RCCService

COPY ./RCCService /RCCService

ENTRYPOINT ["Start.bat"]
