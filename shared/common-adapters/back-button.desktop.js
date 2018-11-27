// @flow
import React, {Component} from 'react'
import type {Props} from './back-button'
import Text from './text'
import Icon from './icon'
import {globalStyles, desktopStyles, collapseStyles} from '../styles'

class BackButton extends Component<Props> {
  onClick(event: SyntheticEvent<>) {
    event.preventDefault()
    event.stopPropagation()
    if (this.props.onClick) {
      this.props.onClick()
    }
  }

  render() {
    return (
      <div style={collapseStyles([styles.container, this.props.style])} onClick={e => this.onClick(e)}>
        <Icon type="iconfont-arrow-left" style={styles.icon} color={this.props.iconColor} />
        {this.props.title !== null && !this.props.hideBackLabel && (
          <Text type="BodyPrimaryLink" style={this.props.textStyle} onClick={e => this.onClick(e)}>
            {this.props.title || 'Back'}
          </Text>
        )}
      </div>
    )
  }
}

export const styles = {
  container: {
    ...globalStyles.flexBoxRow,
    ...desktopStyles.clickable,
    alignItems: 'center',
    zIndex: 1,
  },
  icon: {
    marginRight: 6,
  },
}

export default BackButton
