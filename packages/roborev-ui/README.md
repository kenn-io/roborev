# `@kenn-io/roborev-ui`

Network-free Svelte presentation components for Roborev review content.

This package ships Svelte and TypeScript source. Consumers compile that source
as part of their Svelte application. The supported API includes the complete
`ReviewProjectionView` plus composable review content, metadata, panel,
response, verdict, and status components.

All components are props-in/render-out. The package does not fetch daemon data,
subscribe to streams, or perform review mutations.
