---
layout: "docs"
page_title: "Plugins"
sidebar_current: "docs-internals-plugins"
description: |-
  Learn about how external plugins work in Nomad.
---

# Plugins

Nomad 0.9 introduced a plugin framework which allows users to extend the
functionality of some components within Nomad. The design of the plugin system
is inspired by the lessons learned from plugin systems implemented in other
HashiCorp products such as Terraform and Vault.

The following components are currently plugable within Nomad:

- [Task Drivers](/docs/internals/plugins/task-drivers.html)
- [Devices](/docs/internals/plugins/devices.html)

# Architecture

The Nomad plugin framework uses the [go-plugin][goplugin] project to expose
a language independent plugin interface. Plugins implement a set of GRPC
services, and Nomad manages running the plugin and calling the implemented
RPCs. This means that plugins are free to be implemented in the author's
language of choice.

To make plugin development easier, a set of go interfaces and structs exist for
each plugin type that abstract away go-plugin and the GRPC interface. The
guides in this documentation reference these abstractions for ease of use.

[goplugin]: https://github.com/hashicorp/go-plugin