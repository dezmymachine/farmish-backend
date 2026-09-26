# Firebase Auth emulator for local development and tests. Auth only: it needs
# no Java (unlike the Firestore/Database emulators).
FROM node:22-alpine
ARG FIREBASE_TOOLS_VERSION=15.31.0
RUN npm install -g firebase-tools@${FIREBASE_TOOLS_VERSION} && npm cache clean --force \
 && apk add --no-cache curl
WORKDIR /emulator
COPY firebase.json .
EXPOSE 9099
# demo-* project IDs are emulator-only: they can never touch a real project.
ENV GCLOUD_PROJECT=demo-farmish
CMD ["sh", "-c", "firebase emulators:start --only auth --project ${GCLOUD_PROJECT}"]
