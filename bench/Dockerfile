FROM node:22-alpine
WORKDIR /deps
COPY package.json package-lock.json* ./
RUN npm install --no-audit --no-fund && npm ls --depth=0
ENV NODE_PATH=/deps/node_modules
WORKDIR /bench
