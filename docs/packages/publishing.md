# Publishing

ChainLink allows you to publish your project as a package to a package registry.

## Prerequisites

1. **Metadata**: Ensure your `chain.toml` has the `[Package]` section filled out with `Name` and `Version`.
2. **Authentication**: You must be logged in to the registry.
   ```bash
   chainlink login
   ```
   This will typically trigger a GitHub device flow or prompt for an API key.

## The Publishing Process

When you run `chainlink publish`, the following happens:

1. **Packaging**: ChainLink creates a temporary directory and copies the `src` and `packages` folders into it.
2. **Manifest Generation**: It automatically generates a `wally.toml` file based on the metadata in your `chain.toml`.
3. **Compression**: The package is zipped.
4. **Upload**: The zip file is uploaded to the registry's `/v1/publish` endpoint with the appropriate `Authorization` header.

## Registry Configuration

By default, ChainLink uses the standard package index. However, you can specify a custom registry in your `chain.toml` (though the current CLI implementation primarily derives the API from the registry index config).

## Constraints
- **Size Limit**: Packages must be under 2MB.
- **Version Conflict**: You cannot publish a version that already exists in the registry.
