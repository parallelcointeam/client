// @flow
import * as React from 'react'
import * as Kb from '../common-adapters/mobile.native'
import * as Suggestors from '../chat/conversation/input-area/suggestors'
import {PeoplePageSearchBar, PeoplePageList} from './index.shared'
import {type Props} from '.'
import {globalStyles, styleSheetCreate} from '../styles'
import {isIOS} from '../constants/platform'

const People = (props: Props) => (
  <Kb.ScrollView
    style={styles.scrollView}
    refreshControl={
      // TODO set refreshing to the actual prop once the bug in RN gets fixed
      // see https://github.com/facebook/react-native/issues/5839
      <Kb.NativeRefreshControl refreshing={isIOS ? false : props.waiting} onRefresh={() => props.getData()} />
    }
  >
    <PeoplePageSearchBar
      {...props}
      styleRowContainer={styles.searchRow}
      styleSearchContainer={styles.searchContainer}
      styleSearchText={styles.searchText}
    />
    <Kb.Avatar
      username={props.myUsername}
      onClick={() => props.onClickUser(props.myUsername)}
      size={32}
      style={Kb.avatarCastPlatformStyles(styles.avatar)}
    />
    <PeoplePageList {...props} />
    <SuggestorTestArea suggestors={['chatUsers']} someOtherProps="hi" />
  </Kb.ScrollView>
)

const _SuggestorTestArea = (props: {...Suggestors.SuggestorHooks, someOtherProps: string}) => (
  <Kb.Box2
    direction="vertical"
    style={{borderColor: 'black', borderStyle: 'solid', borderWidth: 1, margin: 20, padding: 10}}
  >
    <Kb.PlainInput onChangeText={props.onChangeText} onKeyDown={props.onKeyDown} ref={props.inputRef} />
  </Kb.Box2>
)
const SuggestorTestArea = Suggestors.AddSuggestors(_SuggestorTestArea)

const styles = styleSheetCreate({
  avatar: {
    position: 'absolute',
    right: 16,
    top: 8,
    zIndex: 2,
  },
  scrollView: {
    ...globalStyles.fullHeight,
  },
  searchContainer: {
    minHeight: 32,
    width: 200,
  },
  searchRow: {
    left: 0,
  },
  searchText: {
    fontSize: 16,
  },
})

export default People
