# Introduction

ChainLink is a tool and package manager specifically designed for Roblox development. It aims to solve the problem of versioning and managing external tools and libraries across different environments, ensuring that every developer on a project is using the exact same toolset.

## Why ChainLink?

In Roblox development, tools like linters (Selene), formatters, and libraries can vary in version, leading to "it works on my machine" bugs or inconsistent code styles. ChainLink centralizes these dependencies in a `chain.toml` file, allowing for:

- **Deterministic Environments**: All team members use the same tool versions.
- **Simplified Onboarding**: New developers can run `chainlink install` to set up their entire environment.
- **Integrated Publishing**: Easily publish your own libraries to package registries.
- **Transparent Tool Execution**: Use tool proxying to run managed tools as if they were installed globally.

## Core Concepts

### Tools vs. Packages
ChainLink distinguishes between **Tools** (executable binaries used for development, like `selene`) and **Packages** (code libraries used within the project).

### The Registry
ChainLink interacts with registries (like the package index) to resolve and download the correct versions of packages and tools.

### Proxying
One of ChainLink's most powerful features is its ability to act as a proxy. By creating a symlink from a tool's name to the `chainlink` binary, ChainLink can intercept the call, look up the required version in `chain.toml`, and execute the correct binary automatically.
