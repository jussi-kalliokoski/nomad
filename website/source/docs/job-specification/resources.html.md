---
layout: "docs"
page_title: "resources Stanza - Job Specification"
sidebar_current: "docs-job-specification-resources"
description: |-
  The "resources" stanza describes the requirements a task needs to execute.
  Resource requirements include memory, disk space, network, cpu, and more.
---

# `resources` Stanza

<table class="table table-bordered table-striped">
  <tr>
    <th width="120">Placement</th>
    <td>
      <code>job -> group -> task -> **resources**</code>
    </td>
  </tr>
</table>

The `resources` stanza describes the requirements a task needs to execute.
Resource requirements include memory, disk space, network, cpu, and more.

```hcl
job "docs" {
  group "example" {
    task "server" {
      resources {
        cpu    = 100
        disk   = 50
        memory = 256

        network {
          mbits = 100
          port "http" {}
          port "ssh" {
            static = 22
          }
        }
      }
    }
  }
}
```

## `resources` Parameters

- `cpu` `(int: 100)` - Specifies the CPU required to run this task in MHz.

- `disk` `(int: 200)` - Specifies the disk space required in MB.

- `iops` `(int: 0)` - Specifies the number of IOPS required given as a weight
  between 0-1000.

- `memory` `(int: 300)` - Specifies the memory required in MB

- `network` <code>([Network][]: required)</code> - Specifies the network
  requirements, including static and dynamic port allocations.

## `resources` Examples

The following examples only show the `resources` stanzas. Remember that the
`resources` stanza is only valid in the placements listed above.

### Disk Space

This example specifies the task requires 100MB of disk space to operate:

```hcl
resources {
  disk = 100
}
```

### Memory

This example specifies the task requires 2GB of RAM to operate. 2GB is the
equivalent of 2000MB:

```hcl
resources {
  memory = 2000
}
```

### Network

This example shows network constraints as specified in the [network][] stanza
which require 1GBit of bandwidth, dynamically allocates two ports, and
statically allocates one port:

```hcl
resources {
  network {
    mbits = 1000
    port "http" {}
    port "https" {}
    port "lb" {
      static = "8889"
    }
  }
}
```

[network]: /docs/job-specification/network.html "Nomad network Job Specification"
