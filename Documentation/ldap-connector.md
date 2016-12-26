# Authentication through LDAP

## Overview

The LDAP connector allows email/password based authentication, backed by a LDAP directory.

The connector executes two primary queries:

1. Finding the user based on the end user's credentials.
2. Searching for groups using the user entry.

## Security considerations

Dex attempts to bind with the backing LDAP server using the end user's _plain text password_. Though some LDAP implementations allow passing hashed passwords, dex doesn't support hashing and instead _strongly recommends that all administrators just use TLS_. This can often be achieved by using port 636 instead of 389, and administrators that choose 389 are actively leaking passwords.

Dex currently allows insecure connections because the project is still verifying that dex works with the wide variety of LDAP implementations. However, dex may remove this transport option, and _users who configure LDAP login using 389 are not covered by any compatibility guarantees with future releases._

## Configuration

User entries are expected to have an email attribute (configurable through `emailAttr`), and a display name attribute (configurable through `nameAttr`). `*Attr` attributes could be set to "DN" in situations where it is needed but not available elsewhere, and if "DN" attribute does not exist in the record.

The following is an example config file that can be used by the LDAP connector to authenticate a user.

```yaml
connectors:
- type: ldap
  id: ldap
  config:
    # Host and optional port of the LDAP server in the form "host:port".
    # If the port is not supplied, it will be guessed based on "insecureNoSSL".
    # 389 for insecure connections, 636 otherwise.
    host: ldap.example.com:636

    # Following field is required if the LDAP host is not using TLS (port 389).
    # Because this option inherently leaks passwords to anyone on the same network
    # as dex, THIS OPTION MAY BE REMOVED WITHOUT WARNING IN A FUTURE RELEASE.
    # insecureNoSSL: true

    # If a custom certificate isn't provide, this option can be used to turn on
    # TLS certificate checks. As noted, it is insecure and shouldn't be used outside
    # of explorative phases.
    # insecureSkipVerify: true

    # Path to a trusted root certificate file. Default: use the host's root CA.
    rootCA: /etc/dex/ldap.ca

    # A raw certificate file can also be provided inline.
    # rootCAData: ( base64 encoded PEM file )

    # The DN and password for an application service account. The connector uses
    # these credentials to search for users and groups. Not required if the LDAP
    # server provides access for anonymous auth.
    bindDN: uid=seviceaccount,cn=users,dc=example,dc=com
    bindPW: password

    # User search maps a username and password entered by a user to a LDAP entry.
    userSearch:
      # BaseDN to start the search from. It will translate to the query
      # "(&(objectClass=person)(uid=<username>))".
      baseDN: cn=users,dc=example,dc=com
      # Optional filter to apply when searching the directory.
      filter: "(objectClass=person)"

      # username attribute used for comparing user entries. This will be translated
      # and combined with the other filter as "(<attr>=<username>)".
      username: uid
      # The following three fields are direct mappings of attributes on the user entry.
      # String representation of the user.
      idAttr: uid
      # Required. Attribute to map to Email.
      emailAttr: mail
      # Maps to display name of users. No default value.
      nameAttr: name

    # Group search queries for groups given a user entry.
    groupSearch:
      # BaseDN to start the search from. It will translate to the query
      # "(&(objectClass=group)(member=<user uid>))".
      baseDN: cn=groups,dc=freeipa,dc=example,dc=com
      # Optional filter to apply when searching the directory.
      filter: "(objectClass=group)"

      # Following two fields are used to match a user to a group. It adds an additional
      # requirement to the filter that an attribute in the group must match the user's
      # attribute value.
      userAttr: uid
      groupAttr: member

      # Represents group name.
      nameAttr: name
```

The LDAP connector first initializes a connection to the LDAP directory using the `bindDN` and `bindPW`. It then tries to search for the given `username` and bind as that user to verify their password.
Searches that return multiple entries are considered ambiguous and will return an error.

## Example: Mapping a schema to a search config

Writing a search configuration often involves mapping an existing LDAP schema to the various options dex provides. To query an existing LDAP schema install the OpenLDAP tool `ldapsearch`. For `rpm` based distros run:

```
sudo dnf install openldap-clients
```

For `apt-get`:

```
sudo apt-get install ldap-utils
```

For smaller user directories it may be practical to dump the entire contents and search by hand.

```
ldapsearch -x -h ldap.example.org -b 'dc=example,dc=org' | less
```

First, find a user entry. User entries declare users who can login to LDAP connector using username and password.

```
dn: uid=jdoe,cn=users,cn=compat,dc=example,dc=org
cn: Jane Doe
objectClass: posixAccount
objectClass: ipaOverrideTarget
objectClass: top
gidNumber: 200015
gecos: Jane Doe
uidNumber: 200015
loginShell: /bin/bash
homeDirectory: /home/jdoe
mail: jane.doe@example.com
uid: janedoe
```

Compose a user search which returns this user.

```yaml
userSearch:
  # The directory directly above the user entry.
  baseDN: cn=users,cn=compat,dc=example,dc=org
  filter: "(objectClass=posixAccount)"

  # Expect user to enter "janedoe" when logging in.
  username: uid

  # Use the full DN as an ID.
  idAttr: DN

  # When an email address is not available, use another value unique to the user, like uid.
  emailAttr: mail
  nameAttr: gecos
```

Second, find a group entry.

```
dn: cn=developers,cn=groups,cn=compat,dc=example,dc=org
memberUid: janedoe
memberUid: johndoe
gidNumber: 200115
objectClass: posixGroup
objectClass: ipaOverrideTarget
objectClass: top
cn: developers
```

Group searches must match a user attribute to a group attribute. In this example, the search returns users whose uid is found in the group's list of memberUid attributes.

```yaml
groupSearch:
  # The directory directly above the group entry.
  baseDN: cn=groups,cn=compat,dc=example,dc=org
  filter: "(objectClass=posixGroup)"

  # The group search needs to match the "uid" attribute on
  # the user with the "memberUid" attribute on the group.
  userAttr: uid
  groupAttr: memberUid

  # Unique name of the group.
  nameAttr: cn
```

## Example: Searching a FreeIPA server with groups

The following configuration will allow the LDAP connector to search a FreeIPA directory using an LDAP filter.

```yaml

connectors:
- type: ldap
  id: ldap
  config:
    # host and port of the LDAP server in form "host:port".
    host: freeipa.example.com:636
    # freeIPA server's CA
    rootCA: ca.crt
    userSearch:
      # Would translate to the query "(&(objectClass=person)(uid=<username>))".
      baseDN: cn=users,dc=freeipa,dc=example,dc=com
      filter: "(objectClass=posixAccount)"
      username: uid
      idAttr: uid
      # Required. Attribute to map to Email.
      emailAttr: mail
      # Entity attribute to map to display name of users.
    groupSearch:
      # Would translate to the query "(&(objectClass=group)(member=<user uid>))".
      baseDN: cn=groups,dc=freeipa,dc=example,dc=com
      filter: "(objectClass=group)"
      userAttr: uid
      groupAttr: member
      nameAttr: name
```

If the search finds an entry, it will attempt to use the provided password to bind as that user entry.
