// @flow
import * as React from 'react'
import {globalStyles, globalMargins} from '../../styles'
import {Box, Button, Text, ButtonBar} from '../../common-adapters'

export type Props = {
  onCancel: () => void,
  onDBNuke: () => void,
}

function DBNukeConfirm(props: Props) {
  return (
    <Box
      style={{
        ...globalStyles.flexBoxColumn,
        alignItems: 'center',
        flex: 1,
        justifyContent: 'center',
        padding: globalMargins.medium,
      }}
    >
      <Text type="Header" style={{marginTop: globalMargins.medium, textAlign: 'center', width: 320}}>
        Are you sure you want to blast away your local database?
      </Text>
      <ButtonBar>
        <Button type="Secondary" label="Cancel" onClick={props.onCancel} />
        <Button type="Danger" label="Yes, blow it away" onClick={props.onDBNuke} />
      </ButtonBar>
    </Box>
  )
}

export default DBNukeConfirm
